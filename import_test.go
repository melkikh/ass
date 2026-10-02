package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func jsonLines(t *testing.T, values ...object) string {
	t.Helper()
	var b bytes.Buffer
	for _, v := range values {
		if err := json.NewEncoder(&b).Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

func importFixture(t *testing.T) (file, root, cwd string) {
	t.Helper()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	file, root, cwd = filepath.Join(dir, testID+".jsonl"), filepath.Join(dir, "imports"), filepath.Join(dir, "project '\u044e")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, jsonLines(t,
		object{"type": "foreign-meta", "cwd": "/old"},
		object{"type": "ai-title", "aiTitle": "title \"\u044e\" <&>"},
		object{"type": "user", "uuid": testID, "timestamp": "old", "message": object{"content": "hello\nworld"}},
		object{"type": "assistant", "message": object{"content": []any{
			object{"type": "tool_use", "input": object{"n": json.Number("9007199254740993")}},
			object{"type": "text", "text": "answer"}, object{"type": "text", "text": ""},
		}}},
	))
	return
}

func records(t *testing.T, path string) []object {
	t.Helper()
	var rows []object
	if err := scanJSON(testContext, path, func(_ int, v object) error { rows = append(rows, v); return nil }); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPrepareImport(t *testing.T) {
	for _, source := range []string{"claude", "cursor", "opencode"} {
		t.Run(source, func(t *testing.T) {
			file, root, cwd := importFixture(t)
			original, _ := os.ReadFile(file)
			p, err := prepareImport(testContext, source, file, root, cwd)
			if err != nil {
				t.Fatal(err)
			}
			if p.Title != "title \"\u044e\" <&>" || p.CWD != cwd {
				t.Fatalf("wrong import metadata: %+v", p)
			}
			rows := records(t, p.Path)
			if source == "claude" {
				want := records(t, file)
				for _, v := range want {
					v["cwd"] = cwd
				}
				if !reflect.DeepEqual(rows, want) {
					t.Fatal("Claude records or number precision changed")
				}
			} else {
				if len(rows) != 3 || str(rows[0]["type"]) != "ai-title" {
					t.Fatal("foreign transcript includes metadata or dropped title")
				}
				for i, row := range rows[1:] {
					if !uuidRE.MatchString(str(row["uuid"])) || row["sessionId"] != testID || row["cwd"] != cwd {
						t.Fatal("invalid stable message identity")
					}
					content := arr(obj(row["message"])["content"])
					if len(content) != 1 || str(obj(content[0])["type"]) != "text" {
						t.Fatal("foreign import did not retain text-only dialogue")
					}
					want := []string{testID[:24] + "000000000002", testID[:24] + "000000000003"}[i]
					if row["uuid"] != want {
						t.Fatal("message numbering changed")
					}
				}
			}
			info, err := os.Stat(p.Path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("snapshot is not 0600")
			}
			again, err := prepareImport(testContext, source, file, root, cwd)
			if err != nil || again.Path != p.Path {
				t.Fatalf("repeat created another import: %v", err)
			}
			legacy := filepath.Join(filepath.Dir(p.Home), "legacy-revision")
			if err := os.Rename(p.Home, legacy); err != nil {
				t.Fatal(err)
			}
			relative, _ := filepath.Rel(p.Home, p.Path)
			legacyPath := filepath.Join(legacy, relative)
			if source != "claude" {
				for _, row := range rows {
					if row["type"] == "user" || row["type"] == "assistant" {
						row["uuid"], row["timestamp"] = "legacy-uuid", "legacy-time"
					}
				}
				writeFile(t, legacyPath, jsonLines(t, rows...))
				now := time.Now().Add(time.Hour)
				if err := os.Chtimes(file, now, now); err != nil {
					t.Fatal(err)
				}
			}
			again, err = prepareImport(testContext, source, file, root, cwd)
			if err != nil || again.Path != legacyPath {
				t.Fatalf("legacy import identity lost: %v", err)
			}
			moved, err := prepareImport(testContext, source, file, root, t.TempDir())
			if err != nil || moved.Path == legacyPath {
				t.Fatalf("project change reused wrong snapshot: %v", err)
			}
			got, _ := os.ReadFile(file)
			if !bytes.Equal(got, original) {
				t.Fatal("source file was modified")
			}
			writeFile(t, file, string(original)+jsonLines(t, object{"type": "user", "message": object{"content": "new turn"}}))
			appended, err := prepareImport(testContext, source, file, root, cwd)
			if err != nil || appended.Path == legacyPath {
				t.Fatalf("new dialogue reused old snapshot: %v", err)
			}
			assertNoPrepareFiles(t, root)
		})
	}
}

func TestLongImportRecord(t *testing.T) {
	file, root, cwd := importFixture(t)
	text := strings.Repeat("long record ", 100000)
	writeFile(t, file, jsonLines(t, object{"type": "user", "message": object{"content": text}}))
	p, err := prepareImport(testContext, "cursor", file, root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	rows := records(t, p.Path)
	if got := str(obj(arr(obj(rows[0]["message"])["content"])[0])["text"]); got != text {
		t.Fatal("large dialogue record was truncated")
	}
}

func assertNoPrepareFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".prepare.") || d.Name() == "canonical" || d.Name() == "previous" {
			t.Errorf("temporary transcript copy leaked: %s", path)
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil || info.Mode().Perm() != 0700 {
				t.Errorf("import directory is not private: %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareFailureCleanup(t *testing.T) {
	for _, text := range []string{
		"{broken\n", "null\n", "{} garbage\n",
		jsonLines(t, object{"type": "ai-title", "aiTitle": "empty"}),
		jsonLines(t, object{"type": "user", "message": object{"content": 123}}),
	} {
		file, root, cwd := importFixture(t)
		writeFile(t, file, text)
		if _, err := prepareImport(testContext, "cursor", file, root, cwd); err == nil {
			t.Fatal("invalid import succeeded")
		}
		assertNoPrepareFiles(t, root)
	}
	file, root, cwd := importFixture(t)
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	if _, err := prepareImport(ctx, "claude", file, root, cwd); err == nil {
		t.Fatal("canceled import succeeded")
	}
	assertNoPrepareFiles(t, root)
}

func TestMetadataZshBoundary(t *testing.T) {
	for _, source := range []string{"claude", "codex", "cursor", "opencode"} {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.jsonl")
			cwd, native := "/space '\u044e\tline\n", "ses_synthetic_123"
			writeFile(t, path, jsonLines(t, object{
				"type": "session_meta", "cwd": cwd, "payload": object{"cwd": cwd}, "nativeSessionId": native,
			})+"broken later line\n")
			var b bytes.Buffer
			if err := sessionMetadata(testContext, source, path, &b); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("zsh", "-f", "-c", "metadata=$(cat); meta=(\"$"+"{(@0)metadata}\"); printf '%s\\0%s\\0' \"$meta[1]\" \"$meta[2]\"")
			cmd.Stdin = &b
			got, err := cmd.Output()
			if err != nil || string(got) != cwd+"\x00"+native+"\x00" {
				t.Fatalf("metadata damaged at shell boundary: %v", err)
			}
		})
	}
}

func TestMetadataMissingAndMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.jsonl")
	for _, text := range []string{"broken", "{\"cwd\":\"bad\\u0000path\"}"} {
		writeFile(t, path, text)
		if err := sessionMetadata(testContext, "claude", path, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	writeFile(t, path, strings.Repeat("{}\n", 20)+"broken later\n")
	var b bytes.Buffer
	if err := sessionMetadata(testContext, "claude", path, &b); err != nil || b.String() != "\x00\x00" {
		t.Fatal("missing cwd must stay empty without scanning the dialogue")
	}
}
