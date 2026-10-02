package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Cache paths and the import source are persistent identities.
const cacheName = "cs"
const indexName = "index-v5.sqlite"
const lockName = "index.lock"
const importSource = "cs"
const claudeDir = ".claude"

type config struct{ Cache, Codex, Claude, Cursor, OpenCode string }

func settings() (config, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return config{}, fmt.Errorf("home directory: %w", err)
	}
	c := config{
		Cache:    filepath.Join(envOr("XDG_CACHE_HOME", filepath.Join(h, ".cache")), cacheName),
		Codex:    filepath.Join(envOr("CODEX_HOME", filepath.Join(h, ".codex")), "sessions"),
		Claude:   filepath.Join(envOr("CLAUDE_CONFIG_DIR", filepath.Join(h, claudeDir)), "projects"),
		Cursor:   envOr("ASS_CURSOR_DB", filepath.Join(h, "Library/Application Support/Cursor/User/globalStorage/state.vscdb")),
		OpenCode: envOr("ASS_OPENCODE_DB", filepath.Join(envOr("XDG_DATA_HOME", filepath.Join(h, ".local/share")), "opencode", "opencode.db")),
	}
	// The picker changes cwd to keep Unix socket names short. Resolve paths now.
	for _, p := range []*string{&c.Cache, &c.Codex, &c.Claude, &c.Cursor, &c.OpenCode} {
		if *p, err = filepath.Abs(*p); err != nil {
			return config{}, fmt.Errorf("resolve session paths: %w", err)
		}
	}
	return c, nil
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func snapshotProjects(home string) string {
	return filepath.Join(home, claudeDir, "projects")
}
