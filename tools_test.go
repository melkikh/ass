package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJSONLToolTraffic(t *testing.T) {
	for _, source := range []string{"codex", "claude"} {
		t.Run(source, func(t *testing.T) {
			s := testStore(t)
			var rows []object
			var path, prefix string
			if source == "codex" {
				path = filepath.Join(s.cfg.Codex, "fixture.jsonl")
				prefix = codexHeader()
				rows = []object{
					{"type": "message", "role": "user", "content": `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`},
					{"type": "message", "role": "user", "content": `<external_codex_apps_open_page>{"page_id":"excluded"}</external_codex_apps_open_page><user_instructions>excluded</user_instructions>request`},
					{"type": "message", "role": "assistant", "content": []any{object{"type": "text", "text": "Discuss <environment_context>literally</environment_context>"}, object{"type": "image", "text": "excluded"}}},
					{"type": "message", "role": "user", "isMeta": true, "content": "excluded"},
					{"type": "message", "role": "developer", "content": "excluded"},
					{"type": "reasoning", "text": "excluded"},
					{"type": "function_call", "name": "shell", "arguments": `{"cmd":"argument"}`, "metadata": "excluded"},
					{"type": "function_call_output", "output": []any{object{"type": "input_text", "text": "functionresult"}, object{"type": "input_image", "image_url": "excluded"}}},
					{"type": "custom_tool_call", "name": "apply_patch", "input": "patchinput"},
					{"type": "custom_tool_call_output", "output": object{"content": []any{object{"type": "text", "text": "patchresult"}}, "metadata": "excluded"}},
					{"type": "tool_search_call", "arguments": object{"query": "toolquery"}},
					{"type": "tool_search_output", "tools": []any{object{"type": "function", "name": "discoveredtool", "description": "tooldescription", "metadata": "excluded"}}},
					{"type": "web_search_call", "action": object{"type": "search", "queries": []any{"webquery", "secondquery"}, "metadata": "excluded"}},
				}
				for i, row := range rows {
					rows[i] = object{"type": "response_item", "payload": row}
				}
				rows = append(rows, object{"type": "web_search_call", "payload": object{"query": "legacyquery", "url": "https://example.test/legacy"}})
			} else {
				path = filepath.Join(s.cfg.Claude, "project", testID+".jsonl")
				rows = []object{
					{"type": "user", "message": object{"content": "<INSTRUCTIONS>excluded</INSTRUCTIONS>request"}},
					{"type": "user", "isMeta": true, "message": object{"content": "excluded"}},
					{"type": "assistant", "message": object{"content": []any{
						object{"type": "text", "text": "Discuss <environment_context>literally</environment_context>"},
						object{"type": "thinking", "thinking": "excluded"},
						object{"type": "tool_use", "name": "shell", "input": object{"cmd": "argument"}},
					}}},
					{"type": "user", "message": object{"content": []any{
						object{"type": "tool_result", "content": "<environment_context>resultliteral</environment_context>"},
						object{"type": "tool_result", "text": "fallbackresult", "is_error": true},
						object{"type": "tool_result", "content": object{"content": []any{object{"type": "text", "text": "nestedresult"}, object{"type": "image", "text": "excluded"}}}, "metadata": "excluded"},
					}}},
				}
			}
			var data strings.Builder
			data.WriteString(prefix)
			enc := json.NewEncoder(&data)
			for _, row := range rows {
				if err := enc.Encode(row); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, path, data.String())
			update(t, s)
			v := indexed(t, s, source)
			plain := bodyOf(t, s, v, false)
			if plain != " request Discuss <environment_context>literally</environment_context>" || title(v) != "request" {
				t.Fatalf("dialogue/title: %q / %q", plain, title(v))
			}
			all := bodyOf(t, s, v, true)
			want := []string{"shell", "argument"}
			if source == "codex" {
				want = append(want, "functionresult", "apply_patch", "patchinput", "patchresult", "tool_search", "toolquery", "discoveredtool", "tooldescription", "web_search", "webquery", "secondquery", "legacyquery", "https://example.test/legacy")
			} else {
				want = append(want, "<environment_context>resultliteral</environment_context>", "fallbackresult", "nestedresult")
			}
			for _, text := range want {
				if !strings.Contains(all, text) {
					t.Errorf("missing tool text %q in %q", text, all)
				}
			}
			if strings.Contains(all, "excluded") {
				t.Fatal("indexed non-dialogue metadata or media")
			}
			original, err := os.ReadFile(path)
			if err != nil || string(original) != data.String() {
				t.Fatal("modified source transcript", err)
			}
		})
	}
}

func TestToolFieldLimit(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "fixture.jsonl")
	output, _ := json.Marshal(object{"type": "response_item", "payload": object{"type": "function_call_output", "output": []any{object{"type": "text", "text": strings.Repeat("\u754c", toolLimit)}}}})
	writeFile(t, p, codexHeader()+string(output)+"\n"+codexMessage("assistant", "visible fallback"))
	update(t, s)
	v := indexed(t, s, "codex")
	var text string
	if err := s.db.QueryRow("SELECT text FROM chunks WHERE session=? AND tool=1", v.Key).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if len(text) > toolLimit || !utf8.ValidString(text) || !strings.HasSuffix(text, truncated) {
		t.Fatal("tool limit split UTF-8, exceeded the cap or lost its marker")
	}
	if title(v) != "visible fallback" {
		t.Fatal("tool output leaked into the dialogue title")
	}
}

func TestPreviewWithoutSelection(t *testing.T) {
	s := testStore(t)
	for _, selection := range [][2]string{{"", ""}, {"codex", ""}, {"", testID}, {"codex", testID}} {
		var out bytes.Buffer
		if err := s.preview(testContext, selection[0], selection[1], "", false, &out); err != nil {
			t.Errorf("preview for an empty or removed selection: %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("empty selection produced a preview: %q", out.String())
		}
	}
}

func TestIndexRebuild(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(s.cfg.Codex, "fixture.jsonl")
	writeFile(t, path, codexHeader()+codexMessage("user", "unchanged source"))
	update(t, s)
	keep := filepath.Join(s.cfg.Cache, "imports", "keep")
	writeFile(t, keep, "import identity")
	if _, err := s.db.Exec("UPDATE chunks SET text='stale metadata'; UPDATE terms SET body='stale metadata'; INSERT INTO sources VALUES('cursor','old fingerprint'); PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	reopened, err := openStore(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	for _, table := range []string{"sessions", "chunks", "sources", "terms"} {
		var count int
		if err := reopened.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("old %s retained: %d, %v", table, count, err)
		}
	}
	if update(t, reopened) != 1 || bodyOf(t, reopened, indexed(t, reopened, "codex"), false) != " unchanged source" {
		t.Fatal("unchanged source was not reindexed")
	}
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("index version was not updated", err)
	}
	if b, err := os.ReadFile(keep); err != nil || !bytes.Equal(b, []byte("import identity")) {
		t.Fatal("rebuild changed imports", err)
	}
}

func TestCodexIndexRebuild(t *testing.T) {
	s := testStore(t)
	writeFile(t, filepath.Join(s.cfg.Codex, "fixture.jsonl"), codexHeader()+codexMessage("user", "codex dialogue"))
	writeFile(t, filepath.Join(s.cfg.Claude, "project", testID+".jsonl"), `{"type":"user","message":{"content":"claude dialogue"}}`+"\n")
	update(t, s)
	if _, err := s.db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	reopened, err := openStore(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	if rows, err := reopened.existing(testContext, "codex"); err != nil || len(rows) != 0 {
		t.Fatal("old Codex identities retained", err)
	}
	if bodyOf(t, reopened, indexed(t, reopened, "claude"), false) != " claude dialogue" {
		t.Fatal("Codex repair cleared another source")
	}
	if update(t, reopened) != 1 {
		t.Fatal("Codex history was not reindexed")
	}
	second, err := openStore(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.db.Close()
	if bodyOf(t, second, indexed(t, second, "codex"), false) != " codex dialogue" {
		t.Fatal("opening the current index triggered another rebuild")
	}
}
