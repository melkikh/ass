package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testID = "12345678-1234-1234-1234-123456789abc"

var testContext = context.Background()

func testStore(t *testing.T) *store {
	t.Helper()
	root := t.TempDir()
	c := config{Cache: filepath.Join(root, "cache"), Codex: filepath.Join(root, "codex"), Claude: filepath.Join(root, "claude"), Cursor: filepath.Join(root, "cursor.db"), OpenCode: filepath.Join(root, "opencode.db")}
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}
func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
func codexHeader() string {
	return `{"type":"session_meta","payload":{"id":"` + testID + `","cwd":"/tmp/project"}}` + "\n"
}
func codexMessage(role, text string) string {
	b, _ := json.Marshal(object{"type": "response_item", "payload": object{"type": "message", "role": role, "content": []any{object{"type": "input_text", "text": text}}}})
	return string(b) + "\n"
}
func indexed(t *testing.T, s *store, source string) session {
	t.Helper()
	m, err := s.existing(testContext, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range m {
		return v
	}
	t.Fatal("no indexed session")
	return session{}
}
func update(t *testing.T, s *store) int {
	t.Helper()
	n, err := s.update(testContext)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func bodyOf(t *testing.T, s *store, v session, all bool) string {
	t.Helper()
	b, err := s.body(testContext, v, all)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func search(t *testing.T, s *store, q string, all bool) string {
	t.Helper()
	var b bytes.Buffer
	if err := s.search(testContext, q, all, &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestIncrementalJSONL(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "2026/session.jsonl")
	text := codexHeader() + codexMessage("user", "hello world")
	writeFile(t, p, text)
	if n := update(t, s); n != 1 {
		t.Fatalf("changed=%d", n)
	}
	v := indexed(t, s, "codex")
	var firstKey int64
	if err := s.db.QueryRow("SELECT min(key) FROM chunks WHERE session=? AND role<>'title'", v.Key).Scan(&firstKey); err != nil {
		t.Fatal(err)
	}
	if n := update(t, s); n != 0 {
		t.Fatalf("warm changed=%d", n)
	}
	partial := strings.TrimSuffix(codexMessage("assistant", "second message"), "\n")
	writeFile(t, p, text+partial)
	update(t, s)
	v = indexed(t, s, "codex")
	if v.Offset != int64(len(text)) {
		t.Fatal("partial record was committed")
	}
	writeFile(t, p, text+partial+"\n")
	update(t, s)
	v = indexed(t, s, "codex")
	if got := bodyOf(t, s, v, false); got != " hello world second message" {
		t.Fatal(got)
	}
	var after int64
	s.db.QueryRow("SELECT min(key) FROM chunks WHERE session=? AND role<>'title'", v.Key).Scan(&after)
	if after != firstKey {
		t.Fatal("append reparsed old chunks")
	}
	writeFile(t, p, codexHeader()+codexMessage("user", "new"))
	update(t, s)
	v = indexed(t, s, "codex")
	if got := bodyOf(t, s, v, false); got != " new" {
		t.Fatal("truncate retained stale text:", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	update(t, s)
	var count int
	s.db.QueryRow("SELECT count(*) FROM chunks").Scan(&count)
	if count != 0 {
		t.Fatal("deleted session text retained")
	}
	if err := s.db.QueryRow("SELECT count(*) FROM terms WHERE terms MATCH 'hel'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("deleted FTS entries retained")
	}
}
func TestRewriteAndInvalidRecord(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "s.jsonl")
	text := codexHeader() + codexMessage("user", "first")
	writeFile(t, p, text)
	update(t, s)
	writeFile(t, p, codexHeader()+codexMessage("user", "different")+codexMessage("assistant", "tail"))
	update(t, s)
	if got := bodyOf(t, s, indexed(t, s, "codex"), false); got != " different tail" {
		t.Fatal(got)
	}
	writeFile(t, p, "{broken}\n")
	if _, err := s.update(testContext); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if got := bodyOf(t, s, indexed(t, s, "codex"), false); got != " different tail" {
		t.Fatal("parse failure damaged prior index")
	}
}
func TestFiltersAndAllMode(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "s.jsonl")
	injected := "# AGENTS.md instructions for /tmp\n<INSTRUCTIONS>private instructions</INSTRUCTIONS>\n<environment_context>secret context</environment_context>\nreal request"
	writeFile(t, p, codexHeader()+codexMessage("user", injected)+`{"type":"response_item","payload":{"type":"reasoning","text":"hidden reasoning"}}`+"\n"+`{"type":"response_item","payload":{"type":"custom_tool_call","input":"toolsecret"}}`+"\n")
	update(t, s)
	v := indexed(t, s, "codex")
	if v.Title != "real request" {
		t.Fatal(v.Title)
	}
	if b := bodyOf(t, s, v, false); b != " real request" {
		t.Fatal(b)
	}
	if b := bodyOf(t, s, v, true); b != " real request toolsecret" {
		t.Fatal(b)
	}
	if search(t, s, "toolsecret", false) != "" || search(t, s, "toolsecret", true) == "" {
		t.Fatal("tool mode mismatch")
	}
	if search(t, s, "reasoning", true) != "" {
		t.Fatal("thinking was indexed")
	}
	writeFile(t, filepath.Join(s.cfg.Claude, "p", testID+".jsonl"), `{"type":"user","message":{"content":"untitled dialogue"}}`+"\n")
	update(t, s)
	if search(t, s, "untitled", false) != "" || search(t, s, "untitled", true) == "" {
		t.Fatal("untitled visibility changed")
	}
}
func TestAllModeExpandsSearch(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "s.jsonl")
	call, _ := json.Marshal(object{"type": "response_item", "payload": object{"type": "custom_tool_call", "name": "shell", "input": "toolneedle " + strings.Repeat("\u754c", toolLimit) + " omittedtail"}})
	writeFile(t, p, codexHeader()+codexMessage("user", "dialogue")+string(call)+"\n"+
		codexMessage("assistant", strings.Repeat("x", 51000)+" outsidecap tail")+
		`{"type":"response_item","payload":{"type":"function_call_output","output":"lateoutput"}}`+"\n")
	update(t, s)
	v := indexed(t, s, "codex")
	all := bodyOf(t, s, v, true)
	if !strings.Contains(all, "[truncated]") || strings.Contains(all, "omittedtail") {
		t.Fatal("oversized tool field was not visibly truncated")
	}
	for _, q := range []string{"dialogue", "outsidecap", "!toolneedle", "tail$", "dialogue !lateoutput"} {
		if normal := search(t, s, q, false); normal == "" || search(t, s, q, true) != normal {
			t.Errorf("-a lost or duplicated a dialogue match for %q", q)
		}
	}
	for _, q := range []string{"shell", "toolneedle", "lateoutput", "outsidecap lateoutput"} {
		if search(t, s, q, false) != "" || search(t, s, q, true) == "" {
			t.Errorf("tool search mismatch for %q", q)
		}
	}
	for _, mode := range []bool{false, true} {
		var preview bytes.Buffer
		if err := s.preview(testContext, "codex", testID, "lateoutput", mode, &preview); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(preview.String(), "T: lateoutput") != mode {
			t.Fatalf("tool preview in mode %t: %q", mode, preview.String())
		}
	}
	var preview bytes.Buffer
	if err := s.preview(testContext, "codex", testID, "outsidecap", false, &preview); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.String(), "outsidecap") {
		t.Fatal("preview clipped the match in a long line")
	}
}
func TestSearchCompatibility(t *testing.T) {
	if _, err := exec.LookPath("fzf"); err != nil {
		t.Fatal("fzf required for compatibility test")
	}
	s := testStore(t)
	for i, text := range []string{"alpha beta CAFE café \u041a\u0438\u0440\u0438\u043b\u043b\u0438\u0446\u0430 \u0441\u043b\u043e\u0432\u043e punctuation a.b(c literal* tail", "beta elsewhere", "fuzzy a very long b then c"} {
		id := testID[:35] + string(rune('0'+i))
		p := filepath.Join(s.cfg.Claude, "p", id+".jsonl")
		v := object{"type": "user", "message": object{"content": text}}
		b, _ := json.Marshal(v)
		writeFile(t, p, `{"type":"ai-title","aiTitle":"Title"}`+"\n"+string(b)+"\n")
	}
	update(t, s)
	queries := []string{"alpha", "alpha beta", "beta", "ALPHA", "cafe", "café", "\u041a\u0418\u0420\u0418\u041b\u041b\u0418\u0426\u0410", "\u0441\u043b\u043e\u0432\u043e", "a.b(c", "literal*", "'abc", "!alpha", "alpha | elsewhere", "^Title", "tail$", "a", "al", "alpha\\ beta", "!alpha beta", "'abc !alpha", "'", "$", "^", "foo'bar", "alpha | nosuch"}
	candidates, err := s.candidates(testContext, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			var input strings.Builder
			for _, v := range candidates {
				fmtLine := strings.TrimSuffix(display(v), "\n")
				fields := strings.Split(fmtLine, "\t")
				fields[6] = bodyOf(t, s, v, false)
				input.WriteString(strings.Join(fields, "\t") + "\n")
			}
			cmd := exec.Command("fzf", "--ansi", "--exact", "--delimiter=\t", "--with-nth=5,6,7", "--nth=2,3", "--filter="+q, "--accept-nth=2")
			cmd.Env = filterEnvironment()
			cmd.Stdin = strings.NewReader(input.String())
			expected, e := cmd.Output()
			if e != nil {
				if x, ok := e.(*exec.ExitError); !ok || x.ExitCode() != 1 {
					t.Fatal(e)
				}
			}
			actual := search(t, s, q, false)
			var ids strings.Builder
			for _, line := range strings.Split(strings.TrimSpace(actual), "\n") {
				f := strings.Split(line, "\t")
				if len(f) > 1 {
					ids.WriteString(f[1] + "\n")
				}
			}
			if ids.String() != string(expected) {
				t.Fatalf("IDs got %q want %q", ids.String(), expected)
			}
		})
	}
}
func TestPermissions(t *testing.T) {
	s := testStore(t)
	for _, p := range []string{s.cfg.Cache, filepath.Join(s.cfg.Cache, "index-v5.sqlite")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("public permissions: %s %v", p, info.Mode())
		}
	}
}

func fixtureDB(t *testing.T, p, ddl string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func TestOpenCodeIncremental(t *testing.T) {
	s := testStore(t)
	db := fixtureDB(t, s.cfg.OpenCode, `PRAGMA journal_mode=WAL;
CREATE TABLE session(id TEXT PRIMARY KEY,parent_id TEXT,title TEXT,directory TEXT,time_updated INTEGER);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
INSERT INTO session VALUES('ses_test',NULL,'OpenCode title','/tmp',1),('ses_child','ses_test','Child','/tmp',1);
INSERT INTO message VALUES('m','ses_test',1,1,'{"role":"user"}');
INSERT INTO part VALUES('p','m','ses_test',1,1,'{"type":"text","text":"first version"}');`)
	if update(t, s) != 1 {
		t.Fatal("initial OpenCode scan")
	}
	v := indexed(t, s, "opencode")
	if v.ID != syntheticID("ses_test") || v.Native != "ses_test" {
		t.Fatal("identity mismatch")
	}
	if update(t, s) != 0 {
		t.Fatal("OpenCode cache missed")
	}
	if _, err := db.Exec(`UPDATE part SET data='{"type":"text","text":"second version"}',time_updated=2 WHERE id='p'`); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 {
		t.Fatal("part update without session timestamp missed")
	}
	v = indexed(t, s, "opencode")
	if b := bodyOf(t, s, v, false); b != " second version" {
		t.Fatal(b)
	}
	for i, part := range []object{
		{"type": "tool", "tool": "shell", "state": object{"input": object{"cmd": "toolargument"}, "output": "tooloutput", "error": "toolerror", "metadata": "excluded"}},
		{"type": "text", "text": "excluded", "synthetic": true},
		{"type": "text", "text": "excluded", "ignored": true},
		{"type": "reasoning", "text": "excluded"},
		{"type": "file", "url": "excluded"},
	} {
		b, _ := json.Marshal(part)
		if _, err := db.Exec("INSERT INTO part VALUES(?, 'm', 'ses_test', ?, ?, ?)", i, i+2, i+2, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	update(t, s)
	v = indexed(t, s, "opencode")
	all := bodyOf(t, s, v, true)
	if bodyOf(t, s, v, false) != " second version" || strings.Contains(all, "excluded") {
		t.Fatal("OpenCode metadata leaked into search")
	}
	for _, text := range []string{"shell", "toolargument", "tooloutput", "toolerror"} {
		if !strings.Contains(all, text) {
			t.Errorf("missing OpenCode tool text %q", text)
		}
	}
	dest := filepath.Join(t.TempDir(), v.ID+".jsonl")
	if err := s.export(testContext, "opencode", v.ID, dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil || !bytes.Contains(b, []byte("second version")) {
		t.Fatal("export mismatch", err)
	}
	if bytes.Contains(b, []byte("toolargument")) || bytes.Contains(b, []byte("excluded")) {
		t.Fatal("export included tool traffic or metadata")
	}
	if _, err = db.Exec("DELETE FROM session WHERE id='ses_test'"); err != nil {
		t.Fatal(err)
	}
	update(t, s)
	if search(t, s, "", true) != "" {
		t.Fatal("deleted OpenCode session retained")
	}
}
func TestCursorInPlaceEdit(t *testing.T) {
	s := testStore(t)
	db := fixtureDB(t, s.cfg.Cursor, `PRAGMA journal_mode=WAL;CREATE TABLE cursorDiskKV(key TEXT PRIMARY KEY,value TEXT);`)
	meta := object{"composerId": testID, "name": "Cursor title", "createdAt": 1, "workspaceIdentifier": object{"uri": object{"scheme": "file", "path": "/tmp"}}, "fullConversationHeadersOnly": []any{object{"bubbleId": "b"}}}
	b, _ := json.Marshal(meta)
	if _, err := db.Exec("INSERT INTO cursorDiskKV VALUES(?,?),(?,?)", "composerData:"+testID, string(b), "bubbleId:"+testID+":b", `{"type":1,"text":"hello"}`); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 {
		t.Fatal("initial Cursor scan")
	}
	if update(t, s) != 0 {
		t.Fatal("Cursor cache missed")
	}
	if _, err := db.Exec("UPDATE cursorDiskKV SET value=? WHERE key=?", `{"type":1,"text":"other"}`, "bubbleId:"+testID+":b"); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 {
		t.Fatal("same-size bubble edit missed")
	}
	if b := bodyOf(t, s, indexed(t, s, "cursor"), false); b != " other" {
		t.Fatal(b)
	}
	meta["conversationMap"] = object{"b": object{"type": 1, "text": "<environment_context>excluded</environment_context>other",
		"toolFormerData": object{"name": "shell", "rawArgs": `{"cmd":"cursorargument"}`, "params": object{"text": "excluded"},
			"result": `{"output":"cursoroutput","metadata":"excluded"}`, "error": object{"modelVisibleErrorMessage": "cursorerror", "actualErrorMessageOnlySendFromClientToServerNeverTheOtherWayAroundBecauseThatMayBeASecurityRisk": "excluded"},
			"additionalData": object{"composerData": object{"text": "excluded"}}},
		"toolResults": []any{object{"toolName": "read_file", "args": `{"path":"cursorpath"}`, "content": "cursorcontent", "images": []any{object{"data": "excluded"}}, "attachedCodeChunks": []any{object{"content": "excluded"}}},
			object{"toolName": "opaque_tool", "result": "~excluded", "params": "~excluded"}}}}
	b, _ = json.Marshal(meta)
	if _, err := db.Exec("UPDATE cursorDiskKV SET value=? WHERE key=?", string(b), "composerData:"+testID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM cursorDiskKV WHERE key=?", "bubbleId:"+testID+":b"); err != nil {
		t.Fatal(err)
	}
	update(t, s)
	v := indexed(t, s, "cursor")
	all := bodyOf(t, s, v, true)
	if bodyOf(t, s, v, false) != " other" || strings.Contains(all, "excluded") {
		t.Fatal("Cursor metadata leaked into search")
	}
	for _, text := range []string{"shell", "cursorargument", "cursoroutput", "cursorerror", "read_file", "cursorpath", "cursorcontent", "opaque_tool"} {
		if !strings.Contains(all, text) {
			t.Errorf("missing embedded Cursor tool text %q", text)
		}
	}
}
func TestSubagentHidden(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "s.jsonl")
	writeFile(t, p, strings.Replace(codexHeader(), `"cwd"`, `"source":{"subagent":{}},"cwd"`, 1)+codexMessage("user", "internal"))
	update(t, s)
	if search(t, s, "", false) != "" || search(t, s, "", true) == "" {
		t.Fatal("subagent visibility changed")
	}
}

func TestCodexInheritedMetadata(t *testing.T) {
	s := testStore(t)
	parent := filepath.Join(s.cfg.Codex, "parent.jsonl")
	writeFile(t, parent, codexHeader()+codexMessage("user", "shared request"))
	for i := range 3 {
		id := testID[:35] + string(rune('0'+i))
		header, _ := json.Marshal(object{"type": "session_meta", "payload": object{
			"id": id, "session_id": testID, "cwd": "/tmp/child",
			"source": object{"subagent": object{"thread_spawn": object{"parent_thread_id": testID}}},
		}})
		path := filepath.Join(s.cfg.Codex, id+".jsonl")
		text := string(header) + "\n" + codexHeader() + codexMessage("user", "shared request")
		writeFile(t, path, text)
		update(t, s)
		// Appended history must not change the persisted identity either.
		writeFile(t, path, text+codexHeader()+codexMessage("assistant", "child reply"))
		update(t, s)
		indexed, err := s.existing(testContext, "codex")
		if err != nil {
			t.Fatal(err)
		}
		v := indexed[path]
		if v.ID != id || !v.Internal || v.CWD != "/tmp/child" {
			t.Fatalf("inherited metadata replaced the child header: %+v", v)
		}
		if !strings.Contains(bodyOf(t, s, v, true), "child reply") {
			t.Fatal("child continuation was not indexed")
		}
	}
	if out := search(t, s, "shared request", false); strings.Count(out, "\n") != 1 || !strings.Contains(out, parent) {
		t.Fatal("subagents appeared as ordinary parent sessions")
	}
	if strings.Count(search(t, s, "shared request", true), "\n") != 4 {
		t.Fatal("all mode lost the distinct subagent sessions")
	}
}
func TestFileReplacement(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "s.jsonl")
	writeFile(t, p, codexHeader()+codexMessage("user", "original"))
	update(t, s)
	replacement := p + ".new"
	writeFile(t, replacement, codexHeader()+codexMessage("user", "replacement"))
	if err := os.Rename(replacement, p); err != nil {
		t.Fatal(err)
	}
	update(t, s)
	if b := bodyOf(t, s, indexed(t, s, "codex"), false); b != " replacement" {
		t.Fatal(b)
	}
}
func TestCancellationReleasesWriter(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	s.update(ctx)
	done := make(chan struct{})
	go func() { s.update(testContext); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer lock leaked")
	}
}
