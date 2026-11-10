package main

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// BenchmarkSearch includes SQLite reads, normalization and the real fzf matcher.
// The synthetic corpus is built once, outside the timed sub-benchmarks.
func BenchmarkSearch(b *testing.B) {
	if _, err := exec.LookPath("fzf"); err != nil {
		b.Fatal(err)
	}
	s, err := openStore(b.Context(), config{Cache: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.db.Close() })
	ctx := b.Context()
	for i := 0; i < 1200; i++ {
		v := session{Source: []string{"codex", "claude", "cursor", "opencode"}[i%4], Path: fmt.Sprintf("/synthetic/%04d.jsonl", i)}
		tr := transcript{ID: syntheticID(v.Path), Title: fmt.Sprintf("Project %04d discussion", i), Updated: int64(i)}
		for j := 0; j < 24; j++ {
			role := "assistant"
			if j%3 == 0 {
				role = "user"
			}
			text := fmt.Sprintf("Project %d message %d. ", i, j) + strings.Repeat("Review the service configuration, response format, cache policy, retry budget and deployment plan. Keep compatibility with existing clients.\n", 4)
			if i%7 == 0 && j == 8 {
				text += "Investigate login latency and cookie handling. "
			}
			if i%14 == 0 && j == 5 {
				text += "Add regression test coverage. "
			}
			if i%100 == 0 && j == 11 {
				text += "zephyrneedle tracks this rare case. "
			}
			tr.Records = append(tr.Records, record{Role: role, Text: text})
		}
		for j := 0; j < 4; j++ {
			text := fmt.Sprintf("Project %d tool result %d.\n", i, j) + strings.Repeat("Read the synthetic configuration and summarize the deployment status.\n", 4)
			if j == 0 {
				// Negation still matches the plain dialogue in all mode.
				text += "Synthetic test output."
			}
			tr.Records = append(tr.Records, record{Role: "tool", Text: text, Tool: true})
		}
		if err := s.replaceTranscript(ctx, &v, tr); err != nil {
			b.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, query string
		all         bool
	}{
		{"mixed", "login !test", false},
		{"short", "lo", false},
		{"negative", "!test", false},
		{"negative_all", "!test", true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			// A warm-up also exposes an order-sensitive checksum for comparisons.
			var result bytes.Buffer
			if err := s.search(ctx, tc.query, tc.all, &result); err != nil {
				b.Fatal(err)
			}
			b.Logf("query=%q all=%t rows=%d sha256=%s", tc.query, tc.all, bytes.Count(result.Bytes(), []byte{'\n'}), digest(result.Bytes()))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.search(ctx, tc.query, tc.all, io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
