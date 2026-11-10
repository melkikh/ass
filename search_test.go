package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBodyWriteFailure(t *testing.T) {
	s := testStore(t)
	v := session{Source: "codex", Path: filepath.Join(t.TempDir(), "chat")}
	tr := transcript{ID: testID, Records: []record{{Role: "user", Text: "message\ntext"}}}
	if err := s.replaceTranscript(testContext, &v, tr); err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	r.Close()
	defer w.Close()
	if err := s.writeBody(testContext, v, false, w); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost matcher write failure: %v", err)
	}
}

func TestSearchCandidatesWithExclusions(t *testing.T) {
	s := testStore(t)
	for _, f := range []struct{ name, user, assistant string }{
		{"match", "login flow", "cache handles retries"},
		{"excluded", "login flow", "cache test fixture"},
		{"login-only", "login flow", "refresh credentials"},
		{"unrelated", "build docs", "render html"},
	} {
		v := session{Source: "codex", Path: filepath.Join(t.TempDir(), f.name)}
		tr := transcript{ID: syntheticID(f.name), Title: f.name, Records: []record{
			{Role: "user", Text: f.user}, {Role: "assistant", Text: f.assistant},
		}}
		if err := s.replaceTranscript(testContext, &v, tr); err != nil {
			t.Fatal(err)
		}
	}
	const query = "login cache !test"
	candidates, err := s.candidates(testContext, query, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range candidates {
		names = append(names, v.Title)
	}
	slices.Sort(names)
	// Positive terms may occur in different messages. Exclusions stay with fzf.
	if !slices.Equal(names, []string{"excluded", "match"}) {
		t.Fatalf("wrong conversations reached the matcher: %v", names)
	}
	got := search(t, s, query, false)
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "\t"+syntheticID("match")+"\t") {
		t.Fatalf("matcher did not apply the exclusion: %q", got)
	}
}

func TestSearchSourceFilters(t *testing.T) {
	s := testStore(t)
	for _, f := range []struct {
		source, name, text string
	}{
		{"codex", "Codex", "common alpha phrase @cursor"},
		{"claude", "Claude", "common beta"},
		{"cursor", "Cursor", "common literal @codex @unknown"},
		{"opencode", "OpenCode", "common beta"},
	} {
		v := session{Source: f.source, Path: filepath.Join(t.TempDir(), f.name)}
		tr := transcript{ID: syntheticID(f.name), Title: f.name, Records: []record{{Role: "user", Text: f.text}}}
		if err := s.replaceTranscript(testContext, &v, tr); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"@codex", []string{"Codex"}},
		{"common @claude", []string{"Claude"}},
		{"@cursor common", []string{"Cursor"}},
		{"@opencode common", []string{"OpenCode"}},
		{"@CODEX @codex @claude common", []string{"Claude", "Codex"}},
		{"@codex alpha | beta", []string{"Codex"}},
		{"@claude alpha", nil},
		{"'@codex", []string{"Cursor"}},
		{"@unknown", []string{"Cursor"}},
		{`@codex phrase\ @cursor`, []string{"Codex"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			var got []string
			for _, row := range strings.Split(search(t, s, tc.query, false), "\n") {
				if fields := strings.Split(row, "\t"); len(fields) == 7 {
					got = append(got, fields[5])
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	var preview bytes.Buffer
	if err := s.preview(testContext, "cursor", syntheticID("Cursor"), "@cursor", false, &preview); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.String(), "YOU\x1b[0m\n  common literal") {
		t.Fatalf("source-only query did not show the dialogue: %q", preview.String())
	}
}

func TestPreviewRoleBlocks(t *testing.T) {
	for _, source := range []string{"codex", "claude", "cursor", "opencode"} {
		t.Run(source, func(t *testing.T) {
			s := testStore(t)
			v := session{Source: source, Path: filepath.Join(t.TempDir(), "rollout.jsonl")}
			tr := transcript{ID: testID, Records: []record{
				{Role: "user", Text: "first question\nwith a second line"},
				{Role: "user", Text: "clarification"},
				{Role: "assistant", Text: "first answer\nline 2\nline 3\nline 4\nline 5\nline 6\nunique hit"},
				{Role: "user", Text: "follow-up"},
			}}
			if err := s.replaceTranscript(testContext, &v, tr); err != nil {
				t.Fatal(err)
			}
			badge := strings.Split(search(t, s, "", false), "\t")[4]
			if badge == "" {
				t.Fatal("search result omitted its source badge")
			}
			heading := strings.TrimSuffix(badge, "\x1b[0m") + " " + strings.ToUpper(source) + "\x1b[0m\n"
			var preview bytes.Buffer
			if err := s.preview(testContext, source, testID, "", false, &preview); err != nil {
				t.Fatal(err)
			}
			got := preview.String()
			for _, block := range []string{
				"\x1b[90m● YOU\x1b[0m\n  first question\n  with a second line\n\n  clarification\n\n",
				heading + "  first answer",
				"\n\n\x1b[90m● YOU\x1b[0m\n  follow-up\n",
			} {
				if !strings.Contains(got, block) {
					t.Errorf("missing role block %q in %q", block, got)
				}
			}
			preview.Reset()
			if err := s.preview(testContext, source, testID, "unique", false, &preview); err != nil {
				t.Fatal(err)
			}
			if got = preview.String(); !strings.Contains(got, heading+"  line 4") || strings.Contains(got, "first answer") {
				t.Fatalf("search context lost its speaker or included the omitted start: %q", got)
			}
		})
	}
}

func TestPreviewExclusions(t *testing.T) {
	s := testStore(t)
	v := session{Source: "codex", Path: filepath.Join(t.TempDir(), "rollout.jsonl")}
	tr := transcript{ID: testID, Records: []record{
		{Role: "user", Text: "release notes"},
		{Role: "assistant", Text: "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nlogin screen"},
	}}
	if err := s.replaceTranscript(testContext, &v, tr); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		whole bool
	}{
		{"!test", true},
		{"@codex !test", true},
		{"!test | !missing", true},
		{"login !test", false},
		{"login | missing !test", false},
		{`'login\ screen !test`, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if search(t, s, tc.query, false) == "" {
				t.Fatal("query did not select the chat")
			}
			var preview bytes.Buffer
			if err := s.preview(testContext, "codex", testID, tc.query, false, &preview); err != nil {
				t.Fatal(err)
			}
			got := preview.String()
			if !strings.Contains(got, "login screen") || strings.Contains(got, "release notes") != tc.whole {
				t.Fatalf("wrong context for an exclusion query: %q", got)
			}
		})
	}
}
