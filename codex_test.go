package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real stdio child with synthetic data; tests never start the installed Codex.
func TestRPCProcess(t *testing.T) {
	mode := os.Getenv("ASS_RPC_TEST")
	if mode == "" {
		return
	}
	enc, dec := json.NewEncoder(os.Stdout), json.NewDecoder(os.Stdin)
	send := func(v any) {
		if err := enc.Encode(v); err != nil {
			os.Exit(3)
		}
	}
	sourcePath := os.Getenv("ASS_RPC_PATH")
	var importRequest object
	for {
		var req object
		if err := dec.Decode(&req); err != nil {
			os.Exit(0)
		}
		id, method := req["id"], str(req["method"])
		params := obj(req["params"])
		if method == "initialized" {
			continue
		}
		reply := func(v any) { send(object{"id": id, "result": v}) }
		fail := func(message string) { send(object{"id": id, "error": object{"code": -32000, "message": message}}) }
		if method == "initialize" {
			if want := os.Getenv("ASS_RPC_EXPECT_HOME"); want != "" && os.Getenv("HOME") != want {
				fail("snapshot home was not passed to the child")
				continue
			}
			if want := os.Getenv("ASS_RPC_EXPECT_CODEX_HOME"); want != "" && os.Getenv("CODEX_HOME") != want {
				fail("Codex home was not preserved")
				continue
			}
			if want := os.Getenv("ASS_RPC_EXPECT_CLAUDE_HOME"); want != "" && os.Getenv("CLAUDE_CONFIG_DIR") != want {
				fail("Claude source escaped the selected snapshot")
				continue
			}
			if obj(params["capabilities"])["experimentalApi"] != true || obj(params["clientInfo"])["name"] != "ass" {
				fail("bad initialize")
				continue
			}
			if mode == "init-error" {
				fail("initialize failed")
			} else {
				reply(object{})
			}
			continue
		}
		switch mode {
		case "malformed":
			fmt.Fprintln(os.Stdout, "{broken")
			continue
		case "eof":
			os.Exit(0)
		case "stall":
			time.Sleep(time.Hour)
			continue
		}
		switch method {
		case "externalAgentConfig/detect":
			if mode == "detect-error" {
				fail("detect failed")
			} else if mode == "history" || mode == "missing" || mode == "history-error" {
				reply(object{"items": []any{}})
			} else {
				reply(object{"items": []any{
					object{"itemType": "CONFIG", "description": "must not import"},
					object{"itemType": "SESSIONS", "description": "sessions", "details": object{
						"sessions":     []any{object{"path": "/not/selected"}, object{"path": sourcePath, "title": "old"}},
						"unknownField": "keep",
					}},
				}})
			}
		case "externalAgentConfig/import":
			importRequest = params
			items := arr(params["migrationItems"])
			if len(items) != 1 || params["source"] != "cs" || params["migrationSource"] != "claude-code" ||
				params["providerId"] != os.Getenv("ASS_RPC_PROVIDER") {
				fail("import identity changed")
				continue
			}
			details := obj(obj(items[0])["details"])
			sessions := arr(details["sessions"])
			if len(sessions) != 1 || obj(sessions[0])["path"] != sourcePath ||
				obj(sessions[0])["title"] != "chosen title" || details["unknownField"] != "keep" {
				fail("selected session filter or title changed")
				continue
			}
			if mode == "import-error" {
				fail("import failed")
				continue
			}
			if mode == "missing-id" {
				reply(object{})
				continue
			}
			complete := func(importID, target string) {
				successes := []any{object{"source": "/wrong", "target": "ignore"}, object{"source": sourcePath, "target": target}}
				var failures []any
				if mode == "noop" || mode == "failure" {
					successes = nil
					failures = []any{object{"message": "synthetic failure"}}
				}
				send(object{"method": importCompleted, "params": object{"importId": importID, "itemTypeResults": []any{
					object{"itemType": "SESSIONS", "successes": successes, "failures": failures},
				}}})
			}
			send(object{"method": "irrelevant/notification", "params": object{}})
			complete("unrelated", "ignore")
			if mode == "flood" {
				for i := 0; i < 128; i++ {
					complete("unrelated", "ignore")
				}
			}
			if mode == "early" {
				complete("selected", testID)
			}
			reply(object{"importId": "selected"})
			if mode != "early" {
				target := testID
				if mode == "invalid-target" {
					target = "--bad-option"
				}
				complete("selected", target)
			}
		case "externalAgentConfig/import/readHistories":
			if mode == "history-error" {
				fail("history failed")
				continue
			}
			data := []any{}
			if mode != "failure" && mode != "missing" {
				for _, target := range []string{"old-target", testID} {
					data = append(data, object{"successes": []any{
						object{"itemType": "SESSIONS", "source": "/other", "target": "wrong"},
						object{"itemType": "SESSIONS", "source": sourcePath, "target": target},
					}})
				}
			}
			reply(object{"data": data})
		case "thread/metadata/update":
			if params["threadId"] != testID || params["projectId"] != "project-id" || importRequest == nil {
				fail("bad project assignment")
			} else {
				reply(object{})
			}
		case "project/list":
			if params["limit"] != float64(100) {
				fail("bad project page size")
				continue
			}
			dir := os.Getenv("ASS_RPC_DIR")
			cursor := str(params["cursor"])
			var next any
			if cursor == "" || mode == "cursor-loop" {
				next = "page2"
			}
			reply(object{"nextCursor": next, "data": []any{
				object{"id": "project-id", "name": "Project '\u044e", "roots": []any{object{"path": dir}}},
				object{"id": "bad\tid", "name": "bad", "roots": []any{object{"path": dir}}},
				object{"id": "gone", "name": "gone", "roots": []any{object{"path": filepath.Join(dir, "missing")}}},
			}})
		default:
			fail("unexpected method: " + method)
		}
	}
}

func fakeCodex(t *testing.T, ctx context.Context, mode, source string) *codexClient {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRPCProcess$")
	cmd.Env = append(os.Environ(), "ASS_RPC_TEST="+mode, "ASS_RPC_PATH=/selected.jsonl",
		"ASS_RPC_PROVIDER="+source, "ASS_RPC_DIR="+t.TempDir())
	c, err := startCodexCommand(ctx, cancel, cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.close()
		if c.cmd.ProcessState == nil {
			t.Error("Codex child was not reaped")
		}
	})
	return c
}

func TestCodexImportProtocol(t *testing.T) {
	for _, source := range []string{"claude", "cursor", "opencode"} {
		modes := []string{"late"}
		if source == "claude" {
			modes = []string{"early", "late", "history", "noop", "failure", "missing", "detect-error", "history-error", "import-error", "missing-id", "flood"}
		}
		for _, mode := range modes {
			t.Run(source+"/"+mode, func(t *testing.T) {
				provider := source
				if source == "claude" {
					provider = "claude-code"
				}
				c := fakeCodex(t, testContext, mode, provider)
				id, err := c.importSession(preparedImport{Path: "/selected.jsonl", CWD: "/tmp", Title: "chosen title"}, source)
				switch mode {
				case "early", "late", "history", "noop":
					if err != nil || id != testID {
						t.Fatalf("wrong imported thread: %q %v", id, err)
					}
					if mode != "history" {
						if err := c.call("thread/metadata/update", object{"threadId": id, "projectId": "project-id"}, nil); err != nil {
							t.Fatal(err)
						}
					}
				default:
					if err == nil || id != "" {
						t.Fatal("failed import returned a thread")
					}
				}
			})
		}
	}
}

func TestCodexReaderErrorsAndCancellation(t *testing.T) {
	for _, mode := range []string{"malformed", "eof", "stall"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(testContext, 2*time.Second)
			defer cancel()
			c := fakeCodex(t, ctx, mode, "claude-code")
			if mode == "stall" {
				cancel()
			}
			if err := c.call("project/list", object{}, &object{}); err == nil {
				t.Fatal("reader failure ignored")
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(testContext, 500*time.Millisecond)
		defer cancel()
		c := fakeCodex(t, ctx, "stall", "claude-code")
		err := c.call("project/list", object{}, &object{})
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, io.EOF) {
			t.Fatalf("timeout not propagated: %v", err)
		}
	})
	t.Run("initialize", func(t *testing.T) {
		ctx, cancel := context.WithCancel(testContext)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRPCProcess$")
		cmd.Env = append(os.Environ(), "ASS_RPC_TEST=init-error")
		if _, err := startCodexCommand(ctx, cancel, cmd); err == nil || !strings.Contains(err.Error(), "initialize failed") {
			t.Fatalf("initialization error lost: %v", err)
		}
		if cmd.ProcessState == nil {
			t.Fatal("failed initialization left a child")
		}
	})
}

func TestProjectsProtocolAndRows(t *testing.T) {
	c := fakeCodex(t, testContext, "late", "claude-code")
	entries, err := c.projects()
	if err != nil || len(entries) != 2 {
		t.Fatalf("pagination/filtering failed: %v", err)
	}
	original, current := entries[0].Path, t.TempDir()
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := writeProjects(&b, entries, original, current); err != nil {
		t.Fatal(err)
	}
	if want := "project-id\t" + original + "\tOriginal project\n-\t" + current + "\tCurrent directory\n"; b.String() != want {
		t.Fatalf("wrong priority or duplicate project rows: %q", b.String())
	}
	loop := fakeCodex(t, testContext, "cursor-loop", "claude-code")
	if _, err := loop.projects(); err == nil {
		t.Fatal("repeating page cursor was accepted")
	}
}

func TestImportCommand(t *testing.T) {
	for _, rootMode := range []string{"explicit", "default"} {
		t.Run(rootMode, func(t *testing.T) { testImportCommand(t, rootMode) })
	}
}

func testImportCommand(t *testing.T, rootMode string) {
	file, root, cwd := importFixture(t)
	t.Chdir(t.TempDir())
	caller, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "custom-codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "custom-claude")
	t.Setenv("XDG_CACHE_HOME", "custom-cache")
	if rootMode == "default" {
		root = filepath.Join(caller, "custom-cache/cs/cursor/import")
	}
	sourceRows := records(t, file)
	sourceRows[1]["aiTitle"] = "chosen title"
	writeFile(t, file, jsonLines(t, sourceRows...))
	p, err := prepareImport(testContext, "cursor", file, root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "codex-stub")
	writeFile(t, stub, "#!/bin/sh\nexec \"$ASS_RPC_EXEC\" -test.run=^TestRPCProcess$\n")
	if err := os.Chmod(stub, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASS_CODEX_BIN", stub)
	t.Setenv("ASS_RPC_EXEC", os.Args[0])
	t.Setenv("ASS_RPC_PATH", p.Path)
	t.Setenv("ASS_RPC_PROVIDER", "cursor")
	t.Setenv("ASS_RPC_EXPECT_HOME", p.Home)
	t.Setenv("ASS_RPC_EXPECT_CODEX_HOME", filepath.Join(caller, "custom-codex"))
	t.Setenv("ASS_RPC_EXPECT_CLAUDE_HOME", filepath.Join(p.Home, ".claude"))
	args := []string{"--source", "cursor", "--file", file, "--cwd", cwd, "--project", "project-id"}
	if rootMode == "explicit" {
		args = append(args, "--root", root)
	}
	for _, mode := range []string{"early", "invalid-target"} {
		t.Setenv("ASS_RPC_TEST", mode)
		var b bytes.Buffer
		err := launchCommand(testContext, "import", args, &b)
		if mode == "early" && (err != nil || b.String() != testID+"\n") {
			t.Fatalf("import command failed: %v", err)
		}
		if mode == "invalid-target" && (err == nil || b.Len() != 0) {
			t.Fatal("invalid thread ID crossed the Go/zsh boundary")
		}
	}
	assertNoPrepareFiles(t, root)
}
