package main

import (
	"path/filepath"
	"testing"
)

func TestOpenCodeDatabaseSwitch(t *testing.T) {
	s := testStore(t)
	seed := func(path, text string) {
		t.Helper()
		db := fixtureDB(t, path, `PRAGMA journal_mode=WAL;
CREATE TABLE session(id TEXT PRIMARY KEY,parent_id TEXT,title TEXT,directory TEXT,time_updated INTEGER);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
INSERT INTO session VALUES('ses_shared',NULL,'Shared','/tmp',1);
INSERT INTO message VALUES('m','ses_shared',1,1,'{"role":"user"}');`)
		if _, err := db.Exec(`INSERT INTO part VALUES('p','m','ses_shared',1,1,json_object('type','text','text',?))`, text); err != nil {
			t.Fatal(err)
		}
	}
	seed(s.cfg.OpenCode, "default dialogue")
	if update(t, s) != 1 {
		t.Fatal("default database was not indexed")
	}
	before := indexed(t, s, "opencode")
	alternate := filepath.Join(t.TempDir(), "alternate.db")
	seed(alternate, "configured dialogue")
	s.cfg.OpenCode = alternate
	dest := filepath.Join(t.TempDir(), "selected.jsonl")
	if err := s.export(testContext, "opencode", before.ID, dest); err == nil {
		t.Fatal("export read the previously configured database")
	}
	if update(t, s) != 1 || update(t, s) != 0 {
		t.Fatal("database switch did not reindex exactly once")
	}
	after := indexed(t, s, "opencode")
	if after.ID != before.ID || after.Key != before.Key || after.Path != alternate+"#ses_shared" {
		t.Fatal("database switch changed session identity or retained the old path")
	}
	if bodyOf(t, s, after, false) != " configured dialogue" {
		t.Fatal("cached dialogue did not come from the configured database")
	}
	if err := s.export(testContext, "opencode", after.ID, dest); err != nil {
		t.Fatal(err)
	}
	s.cfg.OpenCode = filepath.Join(t.TempDir(), "broken.db")
	writeFile(t, s.cfg.OpenCode, "not a SQLite database")
	if _, err := s.updateOpenCode(testContext); err == nil || indexed(t, s, "opencode").Path != after.Path {
		t.Fatal("read failure discarded the cached session")
	}
}
