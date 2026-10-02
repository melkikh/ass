package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const maxJSONLine = 64 << 20
const importCompleted = "externalAgentConfig/import/completed"

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type rpcRead struct {
	message rpcMessage
	err     error
}

// One owner sends sequential requests; the reader also retains early completion
// notifications. Closing always cancels, kills and reaps the child and reader.
type codexClient struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	reads     chan rpcRead
	done      chan struct{}
	nextID    int
	completed []json.RawMessage
}

func startCodex(ctx context.Context, sourceHome string) (*codexClient, error) {
	cfg, err := settings()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	cmd := exec.CommandContext(ctx, envOr("ASS_CODEX_BIN", "codex"), "app-server", "--listen", "stdio://")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+filepath.Dir(cfg.Codex))
	if sourceHome != "" {
		// Only the child sees the selected snapshot as its Claude home. Codex's
		// real home must stay pinned so its import history and thread IDs survive.
		cmd.Env = append(cmd.Env, "HOME="+sourceHome, "CLAUDE_CONFIG_DIR="+filepath.Join(sourceHome, claudeDir))
	}
	return startCodexCommand(ctx, cancel, cmd)
}

func startCodexCommand(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd) (*codexClient, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		in.Close()
		out.Close()
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	c := &codexClient{ctx: ctx, cancel: cancel, cmd: cmd, stdin: in, stdout: out, reads: make(chan rpcRead, 1), done: make(chan struct{})}
	go c.read()
	if err = c.call("initialize", object{
		"clientInfo":   object{"name": "ass", "title": "ass session picker", "version": "1"},
		"capabilities": object{"experimentalApi": true},
	}, nil); err == nil {
		err = c.send(object{"method": "initialized", "params": object{}})
	}
	if err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *codexClient) close() {
	c.cancel()
	c.stdin.Close()
	c.cmd.Process.Kill()
	c.stdout.Close()
	<-c.done
	c.cmd.Wait()
}

func (c *codexClient) read() {
	defer close(c.done)
	defer close(c.reads)
	s := bufio.NewScanner(c.stdout)
	s.Buffer(make([]byte, 64<<10), maxJSONLine)
	for s.Scan() {
		var m rpcMessage
		err := json.Unmarshal(s.Bytes(), &m)
		select {
		case c.reads <- rpcRead{m, err}:
		case <-c.ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
	if err := s.Err(); err != nil {
		select {
		case c.reads <- rpcRead{err: err}:
		case <-c.ctx.Done():
		}
	}
}

func (c *codexClient) receive() (rpcMessage, error) {
	select {
	case <-c.ctx.Done():
		return rpcMessage{}, fmt.Errorf("Codex app-server: %w", c.ctx.Err())
	case r, ok := <-c.reads:
		if !ok {
			return rpcMessage{}, fmt.Errorf("Codex app-server: %w", io.EOF)
		}
		if r.err != nil {
			return rpcMessage{}, fmt.Errorf("read Codex app-server: %w", r.err)
		}
		return r.message, nil
	}
}

func (c *codexClient) send(v any) error {
	if err := json.NewEncoder(c.stdin).Encode(v); err != nil {
		return fmt.Errorf("write Codex app-server: %w", err)
	}
	return nil
}

func (c *codexClient) call(method string, params, result any) error {
	c.nextID++
	id := strconv.Itoa(c.nextID)
	req := object{"id": c.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := c.send(req); err != nil {
		return err
	}
	for {
		m, err := c.receive()
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		if m.Method == importCompleted {
			if len(c.completed) >= 128 {
				return fmt.Errorf("too many pending Codex import completions")
			}
			c.completed = append(c.completed, m.Params)
		}
		if string(m.ID) != id || m.Method != "" {
			continue
		}
		if m.Error != nil {
			return fmt.Errorf("Codex %s (%d): %s", method, m.Error.Code, m.Error.Message)
		}
		if len(m.Result) == 0 {
			return fmt.Errorf("Codex %s: missing result", method)
		}
		if result == nil {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(m.Result))
		dec.UseNumber()
		if err := dec.Decode(result); err != nil {
			return fmt.Errorf("Codex %s result: %w", method, err)
		}
		return nil
	}
}

type importSuccess struct {
	ItemType string  `json:"itemType"`
	Source   *string `json:"source"`
	Target   string  `json:"target"`
}

type importCompletion struct {
	ImportID string `json:"importId"`
	Results  []struct {
		ItemType  string          `json:"itemType"`
		Successes []importSuccess `json:"successes"`
		Failures  []struct {
			Message string `json:"message"`
		} `json:"failures"`
	} `json:"itemTypeResults"`
}

func (c *codexClient) completion(id string) (importCompletion, error) {
	for {
		var raw json.RawMessage
		if len(c.completed) > 0 {
			raw, c.completed = c.completed[0], c.completed[1:]
		} else {
			m, err := c.receive()
			if err != nil {
				return importCompletion{}, err
			}
			if m.Method != importCompleted {
				continue
			}
			raw = m.Params
		}
		var result importCompletion
		if err := json.Unmarshal(raw, &result); err != nil {
			return result, fmt.Errorf("read Codex import completion: %w", err)
		}
		if result.ImportID == id {
			return result, nil
		}
	}
}

func (c *codexClient) historyTarget(path string) (string, error) {
	var result struct {
		Data []struct {
			Successes []importSuccess `json:"successes"`
		} `json:"data"`
	}
	if err := c.call("externalAgentConfig/import/readHistories", nil, &result); err != nil {
		return "", err
	}
	for i := len(result.Data) - 1; i >= 0; i-- {
		for _, s := range result.Data[i].Successes {
			if s.ItemType == "SESSIONS" && s.Source != nil && *s.Source == path && s.Target != "" {
				return s.Target, nil
			}
		}
	}
	return "", nil
}

func (c *codexClient) importSession(p preparedImport, source string) (string, error) {
	var detected struct {
		Items []object `json:"items"`
	}
	if err := c.call("externalAgentConfig/detect", object{
		"includeHome": true, "cwds": []string{p.CWD}, "maxSessionAgeDays": 36500,
		"maxSessions": 100000, "migrationSource": "claude-code",
	}, &detected); err != nil {
		return "", err
	}
	var items []object
	for _, item := range detected.Items {
		if str(item["itemType"]) != "SESSIONS" {
			continue
		}
		details := obj(item["details"])
		var sessions []any
		for _, v := range arr(details["sessions"]) {
			s := obj(v)
			if str(s["path"]) == p.Path {
				if p.Title != "" {
					s["title"] = p.Title
				}
				sessions = append(sessions, s)
			}
		}
		if len(sessions) > 0 {
			details["sessions"] = sessions
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		target, err := c.historyTarget(p.Path)
		if err == nil && target == "" {
			err = fmt.Errorf("the selected session was not detected by Codex")
		}
		return target, err
	}
	provider := source
	if source == "claude" {
		provider = "claude-code"
	}
	var response struct {
		ImportID string `json:"importId"`
	}
	// "cs" and the provider names are persistent import identities, not aliases.
	if err := c.call("externalAgentConfig/import", object{
		"migrationItems": items, "source": importSource, "migrationSource": "claude-code", "providerId": provider,
	}, &response); err != nil {
		return "", err
	}
	if response.ImportID == "" {
		return "", fmt.Errorf("Codex did not return an import id")
	}
	completed, err := c.completion(response.ImportID)
	if err != nil {
		return "", err
	}
	for _, r := range completed.Results {
		if r.ItemType != "SESSIONS" {
			continue
		}
		for _, s := range r.Successes {
			if (s.Source == nil || *s.Source == p.Path) && s.Target != "" {
				return s.Target, nil
			}
		}
	}
	target, err := c.historyTarget(p.Path)
	if err != nil || target != "" {
		return target, err
	}
	for _, r := range completed.Results {
		if len(r.Failures) > 0 {
			return "", fmt.Errorf("Codex import: %s", r.Failures[0].Message)
		}
	}
	return "", fmt.Errorf("Codex import returned no thread id")
}
