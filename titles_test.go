package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexNativeTitle(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "rollout.jsonl")
	prompt := codexHeader() + codexMessage("user", "<environment_context>hidden</environment_context>first request")
	writeFile(t, p, prompt)
	index := filepath.Join(filepath.Dir(s.cfg.Codex), "session_index.jsonl")
	names := jsonLines(t, object{"id": testID, "thread_name": "Original title"})
	writeFile(t, index, names)
	update(t, s)
	if search(t, s, "^Original title", false) == "" {
		t.Fatal("native title did not take priority")
	}
	writeFile(t, index, names+jsonLines(t, object{"id": testID, "thread_name": "Renamed chat"}))
	if update(t, s) != 1 || update(t, s) != 0 {
		t.Fatal("rename of an unchanged transcript was missed or repeated")
	}
	if search(t, s, "^Renamed chat", false) == "" || search(t, s, "^Original title", false) != "" {
		t.Fatal("rename did not replace the searchable title")
	}

	writeFile(t, index, "{partial")
	writeFile(t, p, prompt+codexMessage("user", "second request"))
	if n, err := s.update(testContext); n != 1 || err == nil {
		t.Fatalf("invalid names prevented dialogue refresh or went unreported: %d, %v", n, err)
	}
	v := indexed(t, s, "codex")
	if title(v) != "Renamed chat" || bodyOf(t, s, v, false) != " first request second request" {
		t.Fatal("failed title refresh discarded cached name or dialogue")
	}

	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 || search(t, s, "^first request", false) == "" || search(t, s, "^Renamed chat", false) != "" {
		t.Fatal("missing native name did not restore the first request")
	}
}

func TestClaudeNativeTitle(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Claude, "project", testID+".jsonl")
	text := jsonLines(t,
		object{"type": "assistant", "message": object{"content": "assistant prelude"}},
		object{"type": "user", "message": object{"content": "first request"}},
	)
	writeFile(t, p, text)
	update(t, s)
	if title(indexed(t, s, "claude")) != "first request" {
		t.Fatal("Claude fallback was not the first user message")
	}
	text += jsonLines(t, object{"type": "ai-title", "aiTitle": "Generated title"})
	writeFile(t, p, text)
	update(t, s)
	if search(t, s, "^Generated title", false) == "" {
		t.Fatal("generated Claude title did not take priority")
	}
	writeFile(t, p, text+jsonLines(t, object{"type": "custom-title", "customTitle": "Custom title"}))
	update(t, s)
	if search(t, s, "^Custom title", false) == "" || search(t, s, "^Generated title", false) != "" {
		t.Fatal("Claude rename did not replace the searchable title")
	}
}

func TestNativeTitleMigration(t *testing.T) {
	s := testStore(t)
	p := filepath.Join(s.cfg.Codex, "rollout.jsonl")
	writeFile(t, p, codexHeader()+codexMessage("assistant", "prelude")+codexMessage("user", "first request"))
	update(t, s)
	// Version 4 stored the first user prompt as the title and used the first
	// dialogue chunk as its fallback.
	if _, err := s.db.Exec("ALTER TABLE chunks DROP COLUMN flat; UPDATE sessions SET title='first request',fallback='prelude'; PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	reopened, err := openStore(t.Context(), s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	if update(t, reopened) != 0 || search(t, reopened, "^first request", false) == "" {
		t.Fatal("migration lost the fallback or reindexed the Codex transcript")
	}
	writeFile(t, filepath.Join(filepath.Dir(s.cfg.Codex), "session_index.jsonl"),
		jsonLines(t, object{"id": testID, "thread_name": "Native title"}))
	if update(t, reopened) != 1 || search(t, reopened, "^Native title", false) == "" {
		t.Fatal("existing Codex transcript did not get its native name")
	}
}
