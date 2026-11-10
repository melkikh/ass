package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestZshIntegration(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Fatal("zsh is required to verify the launcher")
	}
	executable := filepath.Join(t.TempDir(), "agent's tools", "ass")
	var script bytes.Buffer
	if err := writeZsh(&script, executable); err != nil {
		t.Fatal(err)
	}
	// Loading twice must not recurse through the function or lose a quoted path.
	script.WriteString(script.String())
	script.WriteString("\nprint -r -- $_ASS_BIN; whence -w ass\n")
	cmd := exec.Command("zsh", "-f")
	cmd.Stdin = &script
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell integration cannot load: %v: %s", err, out)
	}
	if string(out) != executable+"\nass: function\n" {
		t.Fatalf("shell integration lost the executable path or function: %s", out)
	}
}

func TestUsageDoesNotOpenCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	t.Setenv("XDG_CACHE_HOME", cache)
	for _, args := range [][]string{nil, {"-a"}, {"unknown"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("unsupported direct invocation succeeded: %v", args)
		}
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("usage error touched the session cache: %v", err)
	}
}

func TestCompleteSource(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{"login @co", "end-of-line+put(dex )"},
		{"@CO", "end-of-line+put(dex )"},
		{"@codex @op", "end-of-line+put(encode )"},
		{"@codex", "end-of-line+put( )"},
		{"@c", "change-footer(Try  @codex @claude @cursor)"},
		{"@", "ignore"},
		{"x)+accept @cu", "end-of-line+put(rsor )"},
		{"@codex ", "toggle+down"},
		{`phrase\ @co`, "toggle+down"},
		{"email@co", "toggle+down"},
	} {
		if got := completeSource(tc.query); got != tc.want {
			t.Errorf("completion for %q = %q, want %q", tc.query, got, tc.want)
		}
	}
}
