package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsPathsAndOverrides(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_DATA_HOME", "XDG_CACHE_HOME", "ASS_CURSOR_DB", "ASS_OPENCODE_DB"} {
		t.Setenv(name, "")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		Cache: filepath.Join(home, ".cache/cs"), Codex: filepath.Join(home, ".codex/sessions"),
		Claude:   filepath.Join(home, ".claude/projects"),
		Cursor:   filepath.Join(home, "Library/Application Support/Cursor/User/globalStorage/state.vscdb"),
		OpenCode: filepath.Join(home, ".local/share/opencode/opencode.db"),
	}
	if c, err := settings(); err != nil || c != want {
		t.Fatalf("default paths changed: %+v, %v", c, err)
	}
	base := t.TempDir()
	t.Chdir(base)
	base, err = os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, "custom '\u044e/"+name)
	}
	want = config{
		Cache: filepath.Join(base, "custom '\u044e/XDG_CACHE_HOME/cs"), Codex: filepath.Join(base, "custom '\u044e/CODEX_HOME/sessions"),
		Claude: filepath.Join(base, "custom '\u044e/CLAUDE_CONFIG_DIR/projects"), Cursor: want.Cursor,
		OpenCode: filepath.Join(base, "custom '\u044e/XDG_DATA_HOME/opencode/opencode.db"),
	}
	if c, err := settings(); err != nil || c != want {
		t.Fatalf("custom homes or relative paths ignored: %+v, %v", c, err)
	}
	t.Setenv("ASS_CURSOR_DB", "new-cursor.db")
	t.Setenv("ASS_OPENCODE_DB", filepath.Join(base, "new-opencode.db"))
	want.Cursor, want.OpenCode = filepath.Join(base, "new-cursor.db"), filepath.Join(base, "new-opencode.db")
	if c, err := settings(); err != nil || c != want {
		t.Fatalf("database overrides did not take precedence: %+v, %v", c, err)
	}
}

func TestRelativeSourceAfterChdir(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "cache")
	t.Setenv("CODEX_HOME", "codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "claude")
	t.Setenv("ASS_CURSOR_DB", "cursor.db")
	t.Setenv("ASS_OPENCODE_DB", "missing-opencode.db")
	c, err := settings()
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	db := fixtureDB(t, c.Cursor, `CREATE TABLE cursorDiskKV(key TEXT PRIMARY KEY,value TEXT);`)
	data := `{"composerId":"` + testID + `","name":"Title","conversation":[{"type":1,"text":"first"}]}`
	if _, err := db.Exec("INSERT INTO cursorDiskKV VALUES(?,?)", "composerData:"+testID, data); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 {
		t.Fatal("initial source scan failed")
	}
	run, err := os.MkdirTemp(c.Cache, "run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(run)
	if update(t, s) != 0 || bodyOf(t, s, indexed(t, s, "cursor"), false) != " first" {
		t.Fatal("changing cwd lost the indexed source")
	}
	if _, err := db.Exec("UPDATE cursorDiskKV SET value=replace(value,'first','other')"); err != nil {
		t.Fatal(err)
	}
	if update(t, s) != 1 || bodyOf(t, s, indexed(t, s, "cursor"), false) != " other" {
		t.Fatal("source changes after chdir were not indexed")
	}
}
