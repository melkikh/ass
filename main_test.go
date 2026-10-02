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
