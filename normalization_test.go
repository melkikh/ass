package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanceledCacheUpgrade(t *testing.T) {
	s := testStore(t)
	v := session{Source: "claude", Path: filepath.Join(t.TempDir(), "session.jsonl")}
	tr := transcript{ID: testID, Title: "Native title", Records: []record{{Role: "user", Text: "retained dialogue"}}}
	if err := s.replaceTranscript(t.Context(), &v, tr); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE chunks DROP COLUMN flat; PRAGMA user_version=5"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if reopened, err := openStore(ctx, s.cfg); !errors.Is(err, context.Canceled) {
		if reopened != nil {
			reopened.db.Close()
		}
		t.Fatalf("canceled upgrade returned %v", err)
	}
	var version, flatColumns int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatalf("canceled upgrade changed schema version: %d, %v", version, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM pragma_table_info('chunks') WHERE name='flat'").Scan(&flatColumns); err != nil || flatColumns != 0 {
		t.Fatalf("canceled upgrade changed the old schema: %d, %v", flatColumns, err)
	}
	reopened, err := openStore(t.Context(), s.cfg)
	if err != nil {
		t.Fatal("retry after cancellation:", err)
	}
	defer reopened.db.Close()
	if got := indexed(t, reopened, "claude"); got != v || bodyOf(t, reopened, got, false) != " retained dialogue" {
		t.Fatal("retry after cancellation changed the chat")
	}
}

func TestNormalizedCacheMigration(t *testing.T) {
	s := testStore(t)
	legacy := "legacy log\u009bin\r\nvisible\ttext"
	var rollout strings.Builder
	rollout.WriteString(codexHeader() + codexMessage("user", legacy))
	for i := 0; i < 300; i++ {
		rollout.WriteString(codexMessage("assistant", fmt.Sprintf("record %d\nsecond\tline", i)))
	}
	writeFile(t, filepath.Join(s.cfg.Codex, "rollout.jsonl"), rollout.String())
	writeFile(t, filepath.Join(filepath.Dir(s.cfg.Codex), "session_index.jsonl"),
		jsonLines(t, object{"id": testID, "thread_name": "Native title"}))
	writeFile(t, filepath.Join(s.cfg.Claude, "project", testID+".jsonl"),
		jsonLines(t, object{"type": "user", "message": object{"content": "retained Claude chat"}}))
	update(t, s)
	before := []session{indexed(t, s, "codex"), indexed(t, s, "claude")}
	wantBody := bodyOf(t, s, before[0], false)
	const keysQuery = "SELECT group_concat(key) FROM (SELECT key FROM chunks ORDER BY key)"
	var keys string
	if err := s.db.QueryRow(keysQuery).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	// Recreate a v5 cache from before terminal-control cleanup, including its FTS.
	if _, err := s.db.Exec("UPDATE chunks SET text=? WHERE session=? AND role='user'", legacy, before[0].Key); err != nil {
		t.Fatal(err)
	}
	oldFlat := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(legacy)
	if _, err := s.db.Exec("UPDATE terms SET body=? WHERE rowid IN (SELECT key FROM chunks WHERE session=? AND role='user')", oldFlat, before[0].Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
INSERT INTO sources VALUES('cursor','retained fingerprint');
ALTER TABLE chunks DROP COLUMN flat;
PRAGMA user_version=5;`); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	for i := 0; i < 2; i++ {
		reopened, err := openStore(t.Context(), s.cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range before {
			if got := indexed(t, reopened, want.Source); got != want {
				t.Fatalf("migration changed session identity, title or offsets: %+v, want %+v", got, want)
			}
		}
		var gotKeys, fingerprint string
		var missing, version int
		if err := reopened.db.QueryRow(keysQuery).Scan(&gotKeys); err != nil || gotKeys != keys {
			t.Fatalf("migration changed chunk identities: %q, %v", gotKeys, err)
		}
		if err := reopened.db.QueryRow("SELECT fingerprint FROM sources WHERE source='cursor'").Scan(&fingerprint); err != nil || fingerprint != "retained fingerprint" {
			t.Fatalf("migration discarded source state: %q, %v", fingerprint, err)
		}
		if err := reopened.db.QueryRow("SELECT count(*) FROM chunks WHERE flat IS NULL").Scan(&missing); err != nil || missing != 0 {
			t.Fatalf("migration left %d unnormalized chunks: %v", missing, err)
		}
		if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
			t.Fatalf("migration version: %d, %v", version, err)
		}
		if got := bodyOf(t, reopened, before[0], false); got != wantBody {
			t.Fatal("migration changed searchable dialogue")
		}
		if search(t, reopened, "login", false) == "" || search(t, reopened, "record 299", false) == "" {
			t.Fatal("migration lost an FTS match or failed to repair legacy controls")
		}
		if err := reopened.db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyWriterAfterMigration(t *testing.T) {
	s := testStore(t)
	v := session{Source: "claude", Path: filepath.Join(t.TempDir(), "session.jsonl")}
	tr := transcript{ID: testID, Title: "Native title", Records: []record{{Role: "user", Text: "first"}}}
	if err := s.replaceTranscript(testContext, &v, tr); err != nil {
		t.Fatal(err)
	}
	// A v5 picker already running during the upgrade can still finish an update.
	r, err := s.db.Exec("INSERT INTO chunks(session,role,tool,all_start,text) VALUES(?,'assistant',0,0,?)", v.Key, "legacy\tmessage\u009b")
	if err != nil {
		t.Fatal(err)
	}
	key, err := r.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO terms(rowid,body) VALUES(?,?)", key, "legacy message\u009b"); err != nil {
		t.Fatal(err)
	}
	if got := bodyOf(t, s, v, false); got != " first legacy message" {
		t.Fatalf("legacy write was lost or emitted controls: %q", got)
	}
	if search(t, s, "legacy", false) == "" {
		t.Fatal("legacy write was not searchable")
	}
}
