package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestClosedWALSource(t *testing.T) {
	s := testStore(t)
	db := fixtureDB(t, s.cfg.Cursor, `PRAGMA journal_mode=WAL; CREATE TABLE cursorDiskKV(key TEXT PRIMARY KEY,value TEXT);`)
	data := `{"composerId":"` + testID + `","name":"Title","conversation":[{"type":1,"text":"closed source"}]}`
	if _, err := db.Exec("INSERT INTO cursorDiskKV VALUES(?,?)", "composerData:"+testID, data); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.cfg.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 || bodyOf(t, s, indexed(t, s, "cursor"), false) != " closed source" {
		t.Fatal("closed WAL source was not indexed")
	}
	if update(t, s) != 0 {
		t.Fatal("unchanged WAL source was rescanned")
	}
	after, err := os.ReadFile(s.cfg.Cursor)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reading changed the source database", err)
	}
}

func TestSourceReadOnlySnapshot(t *testing.T) {
	for _, journal := range []string{"DELETE", "WAL"} {
		t.Run(journal, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.db")
			db := fixtureDB(t, path, `PRAGMA journal_mode=`+journal+`;
CREATE TABLE data(id INTEGER PRIMARY KEY,v TEXT);
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1000)
INSERT INTO data SELECT i,'before' || printf('%0500d',0) FROM n;`)
			db.SetMaxOpenConns(1)
			src, err := openSource(testContext, path)
			if err != nil {
				t.Fatal(err)
			}
			defer src.close()
			read := func(id int) string {
				t.Helper()
				var v string
				if err := src.tx.QueryRow("SELECT substr(v,1,6) FROM data WHERE id=?", id).Scan(&v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			if read(1) != "before" {
				t.Fatal("wrong initial data")
			}
			if _, err := src.tx.Exec("DELETE FROM data"); err == nil {
				t.Fatal("source connection allowed a write")
			}
			_, err = db.Exec("UPDATE data SET v='after!' || printf('%0500d',0)")
			if journal == "DELETE" && err == nil {
				t.Fatal("writer bypassed the source read lock")
			}
			if journal == "WAL" && err != nil {
				t.Fatal(err)
			}
			if read(1000) != "before" {
				t.Fatal("read transaction mixed database generations")
			}
			src.close()
			if _, err := db.Exec("UPDATE data SET v='after!' || printf('%0500d',0)"); err != nil {
				t.Fatal("source close retained a lock:", err)
			}
		})
	}
}

func TestClaudeReadFailurePreservesIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read directories without read permission")
	}
	s := testStore(t)
	project := filepath.Join(s.cfg.Claude, "project")
	writeFile(t, filepath.Join(project, testID+".jsonl"), `{"type":"user","message":{"content":"retained"}}`+"\n")
	update(t, s)
	if err := os.Chmod(project, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(project, 0700) })
	if _, err := s.updateFiles(testContext, "claude", s.cfg.Claude); err == nil {
		t.Fatal("unreadable project was treated as empty")
	}
	if bodyOf(t, s, indexed(t, s, "claude"), false) != " retained" {
		t.Fatal("read failure removed cached dialogue")
	}
}

func TestClaudeProjectSymlink(t *testing.T) {
	s := testStore(t)
	s.cfg.Claude += "[literal]"
	project := t.TempDir()
	writeFile(t, filepath.Join(project, testID+".jsonl"), `{"type":"user","message":{"content":"linked"}}`+"\n")
	if err := os.Mkdir(s.cfg.Claude, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.cfg.Claude, "project")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 || bodyOf(t, s, indexed(t, s, "claude"), false) != " linked" {
		t.Fatal("linked project was not indexed")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 {
		t.Fatal("removed project retained its indexed sessions")
	}
}
