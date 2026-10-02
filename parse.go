package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
var nativeRE = regexp.MustCompile(`^ses_[A-Za-z0-9_-]+$`)
var prefixRE = regexp.MustCompile(`(?s)^\s*# AGENTS\.md instructions[^\n]*\n+\s*<INSTRUCTIONS>.*?</INSTRUCTIONS>\s*`)
var injectedTags = []string{"INSTRUCTIONS", "user_instructions", "environment_context", "recommended_plugins", "permissions instructions", "skills_instructions", "app-context", "in-app-browser-context", "external_codex_apps_open_page", "guardian_tool_descriptions", "task-notification", "turn_aborted", "ide_opened_file", "ide_selection", "local-command-stdout", "local-command-stderr", "local-command-caveat", "command-name", "command-message", "command-args"}
var tagREs = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, tag := range injectedTags {
		out = append(out, regexp.MustCompile(`(?s)^\s*<`+tag+`(?:\s[^>]*)?>.*?</`+tag+`>\s*`))
	}
	return out
}()

func cleanUser(s string) string {
	for {
		old := s
		s = prefixRE.ReplaceAllString(s, "")
		for _, re := range tagREs {
			s = re.ReplaceAllString(s, "")
		}
		if strings.TrimSpace(s) == "<no retained transcript delta entries>" {
			return ""
		}
		if s == old {
			return s
		}
	}
}
func cleanControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' || r == 127 {
			return -1
		}
		return r
	}, s)
}
func flatten(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(cleanControls(s))
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func truncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type object = map[string]any

func obj(v any) object { m, _ := v.(map[string]any); return m }
func str(v any) string { vstr, _ := v.(string); return vstr }
func arr(v any) []any  { a, _ := v.([]any); return a }
func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseRecord(ctx context.Context, tx *sql.Tx, s *session, b []byte) error {
	var v object
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v["isMeta"] == true || obj(v["message"])["isMeta"] == true {
		return nil
	}
	if s.CWD == "" {
		s.CWD = str(v["cwd"])
	}
	kind := str(v["type"])
	if s.Source == "codex" {
		p := obj(v["payload"])
		if p["isMeta"] == true {
			return nil
		}
		switch kind {
		case "session_meta":
			// Rollouts start with their own header. Later headers belong to
			// inherited history and must not replace this thread's identity.
			if s.Offset != 0 {
				return nil
			}
			s.ID = str(p["id"])
			if s.ID == "" {
				s.ID = str(p["session_id"])
			}
			s.CWD = str(p["cwd"])
			_, sub := obj(p["source"])["subagent"]
			s.Internal = sub || str(p["source"]) == "subagent"
		default:
			itemType := str(p["type"])
			if kind != "response_item" {
				itemType = kind
				if p == nil {
					p = v
				}
			}
			switch itemType {
			case "message":
				role := str(p["role"])
				if role != "user" && role != "assistant" {
					return nil
				}
				content := p["content"]
				if text, ok := content.(string); ok {
					content = []any{object{"type": "text", "text": text}}
				}
				for _, x := range arr(content) {
					c := obj(x)
					if str(c["type"]) != "input_text" && str(c["type"]) != "output_text" && str(c["type"]) != "text" {
						continue
					}
					text := str(c["text"])
					if role == "user" {
						text = cleanUser(text)
						if !s.Titled && strings.TrimSpace(text) != "" {
							s.Title = truncate(flatten(text), 120)
							s.Titled = true
						}
					}
					if err := putChunk(ctx, tx, s, role, text, false); err != nil {
						return err
					}
				}
			case "function_call", "custom_tool_call", "tool_search_call":
				input := p["arguments"]
				if input == nil {
					input = p["input"]
				}
				name := str(p["name"])
				if name == "" && itemType == "tool_search_call" {
					name = "tool_search"
				}
				return putChunk(ctx, tx, s, "tool", toolCall(name, input), true)
			case "function_call_output", "custom_tool_call_output", "tool_search_output":
				output := p["output"]
				if output == nil {
					output = p
				}
				return putChunk(ctx, tx, s, "tool", toolText(output), true)
			case "web_search_call":
				input := object{}
				action := obj(p["action"])
				for _, key := range []string{"type", "query", "queries", "url", "urls", "pattern"} {
					x := action[key]
					if x == nil && key != "type" {
						x = p[key]
					}
					if x != nil {
						input[key] = x
					}
				}
				return putChunk(ctx, tx, s, "tool", toolCall("web_search", input), true)
			}
		}
		return nil
	}
	if kind == "ai-title" {
		s.Title = truncate(flatten(str(v["aiTitle"])), 120)
		s.Titled = true
		return nil
	}
	if kind != "user" && kind != "assistant" {
		return nil
	}
	content := obj(v["message"])["content"]
	if t, ok := content.(string); ok {
		return putChunk(ctx, tx, s, kind, t, false)
	}
	for _, x := range arr(content) {
		c := obj(x)
		var err error
		switch str(c["type"]) {
		case "text":
			err = putChunk(ctx, tx, s, kind, str(c["text"]), false)
		case "tool_use":
			err = putChunk(ctx, tx, s, "tool", toolCall(str(c["name"]), c["input"]), true)
		case "tool_result":
			err = putChunk(ctx, tx, s, "tool", toolText(c), true)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func fileAnchor(f *os.File, offset int64) (string, error) {
	// Guard both the beginning and the committed tail: replacement/truncation
	// must not splice old messages into a new transcript with the same path.
	var b []byte
	for _, span := range [][2]int64{{0, min(offset, 256)}, {max(0, offset-256), min(offset, 256)}} {
		buf := make([]byte, span[1])
		n, err := f.ReadAt(buf, span[0])
		if err != nil && err != io.EOF {
			return "", err
		}
		b = append(b, buf[:n]...)
	}
	return digest(b), nil
}
func claudeFiles(ctx context.Context, root string) ([]string, error) {
	projects, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := filepath.Join(root, project.Name())
		info, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".jsonl") {
				paths = append(paths, filepath.Join(dir, file.Name()))
			}
		}
	}
	return paths, nil
}

func (s *store) updateFiles(ctx context.Context, source, root string) (int, error) {
	old, err := s.existing(ctx, source)
	if err != nil {
		return 0, err
	}
	var paths []string
	if source == "claude" {
		// An unreadable project is not evidence that its sessions were deleted.
		paths, err = claudeFiles(ctx, root)
	} else {
		err = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				if p == root && os.IsNotExist(e) {
					return nil
				}
				return e
			}
			if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
				paths = append(paths, p)
			}
			return nil
		})
	}
	if err != nil {
		return 0, err
	}
	changed := 0
	var firstErr error
	for _, p := range paths {
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}
		v, exists := old[p]
		delete(old, p)
		info, e := os.Stat(p)
		if e != nil {
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		if exists && v.Size == info.Size() && v.Mtime == info.ModTime().UnixNano() && v.Inode == inode(info) {
			continue
		}
		if !exists {
			v = session{Source: source, Path: p, ID: strings.TrimSuffix(filepath.Base(p), ".jsonl")}
		}
		if e = s.indexFile(ctx, &v, info); e != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", filepath.Base(p), e)
			}
			continue
		}
		changed++
	}
	n, e := s.removeMissing(ctx, source, old)
	changed += n
	if firstErr == nil {
		firstErr = e
	}
	return changed, firstErr
}
func (s *store) indexFile(ctx context.Context, v *session, info os.FileInfo) error {
	f, err := os.Open(v.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	a, err := fileAnchor(f, v.Offset)
	if err != nil {
		return err
	}
	appendOnly := v.Key != 0 && v.Size < info.Size() && v.Offset <= info.Size() && v.Inode == inode(info) && v.Mtime <= info.ModTime().UnixNano() && a == v.Anchor
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveSession(ctx, tx, v); err != nil {
		return err
	}
	if !appendOnly {
		if err = resetSession(ctx, tx, v); err != nil {
			return err
		}
	}
	if _, err = f.Seek(v.Offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(io.LimitReader(f, info.Size()-v.Offset))
	for {
		line, e := r.ReadBytes('\n')
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if len(strings.TrimSpace(string(line))) > 0 {
			if err = parseRecord(ctx, tx, v, line); err != nil {
				return fmt.Errorf("JSON at byte %d: %w", v.Offset, err)
			}
		}
		v.Offset += int64(len(line))
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	// If a writer appended while we read, the next scan picks up its tail. Do
	// not commit a truncated/replaced file or claim bytes beyond our snapshot.
	now, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Stat(v.Path)
	if err != nil {
		return err
	}
	if now.Size() < info.Size() || inode(current) != inode(info) || now.Size() == info.Size() && !now.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("session changed during read; will retry")
	}
	v.Size = info.Size()
	v.Mtime = info.ModTime().UnixNano()
	v.Inode = inode(info)
	v.Anchor, err = fileAnchor(f, v.Offset)
	if err != nil {
		return err
	}
	if err = finishTitle(ctx, tx, v); err != nil {
		return err
	}
	if err = saveSession(ctx, tx, v); err != nil {
		return err
	}
	return tx.Commit()
}
