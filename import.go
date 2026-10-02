package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type preparedImport struct{ Home, Path, CWD, Title string }

func absolutePath(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(p)
	if os.IsNotExist(err) {
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		resolved, err = absolutePath(parent)
		return filepath.Join(resolved, filepath.Base(p)), err
	}
	return resolved, err
}

func scanJSON(ctx context.Context, path string, visit func(int, object) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), maxJSONLine)
	i := 0
	for s.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(s.Bytes()))
		dec.UseNumber()
		var v object
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("JSON record %d: %w", i+1, err)
		}
		if v == nil {
			return fmt.Errorf("JSON record %d is not an object", i+1)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return fmt.Errorf("JSON record %d has trailing data", i+1)
		}
		if err := visit(i, v); err != nil {
			return err
		}
		i++
	}
	return s.Err()
}

// Hash existing snapshots with the same encoder too: their paths are import
// identities even when a previous helper used different JSON escaping.
func snapshotHash(ctx context.Context, path, source string) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	enc.SetEscapeHTML(false)
	err := scanJSON(ctx, path, func(_ int, v object) error {
		if source != "claude" {
			delete(v, "uuid")
			delete(v, "timestamp")
		}
		return enc.Encode(v)
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func prepareImport(ctx context.Context, source, file, root, cwd string) (p preparedImport, err error) {
	if source != "claude" && source != "cursor" && source != "opencode" {
		return p, fmt.Errorf("unsupported import source %q", source)
	}
	file, err = absolutePath(file)
	if err != nil {
		return p, err
	}
	id := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	if !uuidRE.MatchString(id) {
		return p, fmt.Errorf("invalid source session id")
	}
	p.CWD, err = absolutePath(cwd)
	if err != nil {
		return p, err
	}
	info, err := os.Stat(p.CWD)
	if err != nil || !info.IsDir() {
		return p, fmt.Errorf("selected project directory no longer exists")
	}
	root, err = absolutePath(root)
	if err != nil {
		return p, err
	}
	if err = privateDir(root); err != nil {
		return p, err
	}
	base := filepath.Join(root, id)
	if err = privateDir(base); err != nil {
		return p, err
	}
	work, err := os.MkdirTemp(base, ".prepare.")
	if err != nil {
		return p, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(work)) }()
	temp := filepath.Join(work, "session.jsonl")
	if p.Title, err = stageImport(ctx, source, file, temp, id, p.CWD); err != nil {
		return p, err
	}
	revision, err := snapshotHash(ctx, temp, source)
	if err != nil {
		return p, err
	}
	// Read only this session's revisions. Never export or re-read other chats.
	entries, err := os.ReadDir(base)
	if err != nil {
		return p, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		home := filepath.Join(base, entry.Name())
		projects := snapshotProjects(home)
		dirs, _ := os.ReadDir(projects)
		for _, dir := range dirs {
			candidate := filepath.Join(projects, dir.Name(), id+".jsonl")
			hash, e := snapshotHash(ctx, candidate, source)
			if ctx.Err() != nil {
				return p, ctx.Err()
			}
			if e == nil && hash == revision {
				p.Home, p.Path = home, candidate
				return p, nil
			}
		}
	}
	p.Home = filepath.Join(base, revision[:20])
	p.Path = filepath.Join(snapshotProjects(p.Home), strings.ReplaceAll(strings.TrimPrefix(p.CWD, "/"), "/", "-"), id+".jsonl")
	if err = privateDir(filepath.Dir(p.Path)); err != nil {
		return p, err
	}
	// Publish atomically without replacing an existing import identity.
	if err = os.Link(temp, p.Path); os.IsExist(err) {
		var previous string
		previous, err = snapshotHash(ctx, p.Path, source)
		if err == nil && previous != revision {
			err = fmt.Errorf("import snapshot already exists with different contents")
		}
	}
	return p, err
}

func stageImport(ctx context.Context, source, file, temp, id, cwd string) (title string, err error) {
	info, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	timestamp := info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	dialogue, titled := false, false
	err = scanJSON(ctx, file, func(i int, v object) error {
		kind := str(v["type"])
		if source == "claude" {
			v["cwd"] = cwd
		} else if kind != "ai-title" {
			if kind != "user" && kind != "assistant" {
				return nil
			}
			content := obj(v["message"])["content"]
			if text, ok := content.(string); ok {
				content = []any{object{"type": "text", "text": text}}
			}
			blocks, ok := content.([]any)
			if !ok {
				return fmt.Errorf("invalid dialogue content in record %d", i+1)
			}
			var text []any
			for _, block := range blocks {
				b := obj(block)
				if str(b["type"]) == "text" && str(b["text"]) != "" {
					text = append(text, b)
				}
			}
			if len(text) == 0 {
				return nil
			}
			v = object{"type": kind, "uuid": fmt.Sprintf("%s%012d", id[:24], i), "sessionId": id,
				"cwd": cwd, "timestamp": timestamp, "message": object{"role": kind, "content": text}}
		}
		if kind == "ai-title" && !titled {
			title, titled = str(v["aiTitle"]), true
		}
		if kind == "user" || kind == "assistant" {
			dialogue = true
		}
		return enc.Encode(v)
	})
	if err == nil && !dialogue {
		err = fmt.Errorf("no dialogue to import")
	}
	return title, err
}

func sessionMetadata(ctx context.Context, source, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), maxJSONLine)
	cwd, native := "", ""
	for i := 0; i < 20 && s.Scan(); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		var v object
		if err := json.Unmarshal(s.Bytes(), &v); err != nil {
			return fmt.Errorf("session metadata: %w", err)
		}
		native = str(v["nativeSessionId"])
		if source == "codex" {
			if str(v["type"]) == "session_meta" {
				cwd = str(obj(v["payload"])["cwd"])
			}
		} else {
			cwd = str(v["cwd"])
		}
		if cwd != "" || source == "cursor" || source == "opencode" {
			break
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	if strings.ContainsRune(cwd+native, 0) {
		return fmt.Errorf("NUL in session metadata")
	}
	_, err = fmt.Fprintf(w, "%s\x00%s\x00", cwd, native)
	return err
}
