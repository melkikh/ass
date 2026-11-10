package main

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func chatReference(v session) string {
	switch v.Source {
	case "codex", "claude", "cursor":
		if uuidRE.MatchString(v.ID) {
			if v.Source == "codex" {
				return "codex://threads/" + v.ID
			}
			return v.ID
		}
	case "opencode":
		if nativeRE.MatchString(v.Native) {
			return v.Native
		}
	}
	return ""
}

func writeClipboard(ctx context.Context, ref string) bool {
	var commands [][]string
	switch runtime.GOOS {
	case "darwin":
		commands = [][]string{{"pbcopy"}}
	case "linux":
		// Try Wayland, X11 and the Windows clipboard through WSL.
		commands = [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"clip.exe"}}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = strings.NewReader(ref)
		if cmd.Run() == nil {
			return true
		}
		if ctx.Err() != nil {
			break
		}
	}
	return false
}

func (s *store) copyChat(ctx context.Context, source, id string) string {
	if id == "" {
		return "No chat selected"
	}
	v, err := s.sessionByID(ctx, source, id)
	if err != nil {
		return "Chat reference unavailable"
	}
	ref := chatReference(v)
	if ref == "" {
		return "Chat reference unavailable"
	}
	if !writeClipboard(ctx, ref) {
		return "Clipboard unavailable"
	}
	return "Copied chat reference"
}
