package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Only plain AND terms may narrow the candidate set. fzf remains the authority
// for extended syntax (OR, negation, anchors, escaping, fuzzy and smart case).
var simpleQuery = regexp.MustCompile(`^[a-zA-Z0-9_\- \p{Cyrillic}]+$`)

func candidateTerms(q string) []string {
	if !simpleQuery.MatchString(q) {
		return nil
	}
	var out []string
	for _, t := range strings.Fields(q) {
		r := []rune(t)
		if len(r) >= 3 {
			// Positionless trigrams are only a superset filter. Omitting
			// positions saves substantial disk space on long transcripts.
			out = append(out, string(r[:3]))
			if len(r) > 3 {
				out = append(out, string(r[len(r)-3:]))
			}
		}
	}
	return out
}
func (s *store) candidates(ctx context.Context, q string, all bool) ([]session, error) {
	where := " WHERE id<>''"
	if !all {
		where += " AND titled=1 AND internal=0"
	}
	var args []any
	for _, term := range candidateTerms(q) {
		where += ` AND key IN (SELECT chunks.session FROM terms JOIN chunks ON chunks.key=terms.rowid WHERE terms MATCH ?)`
		args = append(args, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+sessionFields+" FROM sessions"+where+" ORDER BY mtime DESC,path", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session
	for rows.Next() {
		v, e := readSession(rows)
		if e != nil {
			return nil, e
		}
		if uuidRE.MatchString(v.ID) && !strings.ContainsAny(v.Path, "\t\r\n") {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}
func (s *store) body(ctx context.Context, v session, all bool) (string, error) {
	query := "SELECT text FROM chunks WHERE session=? AND role<>'title'"
	if !all {
		query += " AND tool=0"
	}
	query += " ORDER BY key"
	rows, err := s.db.QueryContext(ctx, query, v.Key)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var t string
		if err = rows.Scan(&t); err != nil {
			return "", err
		}
		b.WriteByte(' ')
		b.WriteString(flatten(t))
	}
	return b.String(), rows.Err()
}
func title(v session) string {
	if v.Titled {
		return v.Title
	}
	if v.Fallback != "" {
		return v.Fallback
	}
	return "(empty)"
}
func display(v session) string {
	badge := map[string]string{"codex": "\x1b[36m◆\x1b[0m", "claude": "\x1b[33m◈\x1b[0m", "cursor": "\x1b[35m▣\x1b[0m", "opencode": "\x1b[32m◉\x1b[0m"}[v.Source]
	return fmt.Sprintf("%s\t%s\t%s\t%d\t%s\t%s\t\x1b[90m%s\x1b[0m\n", v.Path, v.ID, v.Source, v.Mtime/1e9, badge, flatten(title(v)), flatten(v.Snippet))
}
func filterEnvironment() []string {
	var out []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "FZF_DEFAULT_OPTS=") || strings.HasPrefix(e, "FZF_DEFAULT_OPTS_FILE=") {
			continue
		}
		out = append(out, e)
	}
	return out
}
func (s *store) search(ctx context.Context, q string, all bool, w io.Writer) error {
	candidates, err := s.candidates(ctx, q, all)
	if err != nil {
		return err
	}
	if strings.TrimSpace(q) == "" {
		for _, v := range candidates {
			if _, err = io.WriteString(w, display(v)); err != nil {
				return err
			}
		}
		return nil
	}
	if len(candidates) == 0 {
		return nil
	}
	// Full bodies go only to the non-interactive matcher, never to the picker,
	// shell variables, argv or temporary files. Results contain just numeric keys.
	cmd := exec.CommandContext(ctx, "fzf", "--ansi", "--exact", "--delimiter=\t", "--with-nth=2,3,4", "--nth=2,3", "--accept-nth=1", "--filter="+q)
	cmd.Env = filterEnvironment()
	// A non-interactive matcher must not share the picker's controlling TTY.
	// Terminal initialization/cleanup in a child must never flush typed input.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	reader, writer := io.Pipe()
	cmd.Stdin = reader
	defer reader.Close()
	writeDone := make(chan error, 1)
	go func() {
		var e error
		defer func() { writer.CloseWithError(e); writeDone <- e }()
		bw := bufio.NewWriter(writer)
		for _, v := range candidates {
			// Negation and end anchors can stop matching when tools are added.
			// Match both bodies so -a always includes ordinary search results.
			for _, mode := range []bool{false, true} {
				if mode && (!all || v.AllBytes == v.PlainBytes) {
					break
				}
				var body string
				body, e = s.body(ctx, v, mode)
				if e != nil {
					return
				}
				_, e = fmt.Fprintf(bw, "%d\t◆\t%s\t\x1b[90m%s\x1b[0m\n", v.Key, flatten(title(v)), body)
				if e != nil {
					return
				}
			}
		}
		e = bw.Flush()
	}()
	output, runErr := cmd.Output()
	reader.Close()
	writeErr := <-writeDone
	if runErr != nil {
		if e, ok := runErr.(*exec.ExitError); !ok || e.ExitCode() != 1 {
			return runErr
		}
	}
	if writeErr != nil {
		return writeErr
	}
	byKey := map[int64]session{}
	for _, v := range candidates {
		byKey[v.Key] = v
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		k, e := strconv.ParseInt(line, 10, 64)
		if e != nil {
			continue
		}
		if v, ok := byKey[k]; ok {
			delete(byKey, k)
			if _, err = io.WriteString(w, display(v)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *store) preview(ctx context.Context, source, id, q string, all bool, w io.Writer) error {
	var key int64
	if err := s.db.QueryRowContext(ctx, "SELECT key FROM sessions WHERE source=? AND id=? ORDER BY mtime DESC LIMIT 1", source, id).Scan(&key); err != nil {
		return err
	}
	query := "SELECT role,text,tool FROM chunks WHERE session=? AND role<>'title'"
	if !all {
		query += " AND tool=0"
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY key", key)
	if err != nil {
		return err
	}
	defer rows.Close()
	terms := strings.Fields(q)
	for i := range terms {
		terms[i] = strings.ToLower(strings.Trim(terms[i], "'!^$"))
	}
	var lines []string
	var selected []int
	var total int
	for rows.Next() {
		var role, text string
		var tool bool
		if err = rows.Scan(&role, &text, &tool); err != nil {
			return err
		}
		prefix := "A: "
		if role == "user" {
			prefix = "U: "
		}
		if tool {
			prefix = "T: "
		}
		for _, line := range strings.Split(prefix+text, "\n") {
			match := strings.TrimSpace(q) == ""
			low := strings.ToLower(line)
			at := 0
			for _, term := range terms {
				if pos := strings.Index(low, term); term != "" && pos >= 0 {
					match = true
					at = pos
					break
				}
			}
			line = previewLine(line, at)
			lines = append(lines, line)
			if match {
				selected = append(selected, len(lines)-1)
				total += len(line)
			}
		}
		if total > 12000 {
			break
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(selected) == 0 {
		_, err = io.WriteString(w, "(no matching lines; the query may match the title)\n")
		return err
	}
	var out bytes.Buffer
	last := -1
	for _, hit := range selected {
		start := max(0, hit-3)
		end := min(len(lines), hit+4)
		if start > last+1 {
			out.WriteString("--\n")
		}
		for i := max(start, last+1); i < end; i++ {
			out.WriteString(lines[i])
			out.WriteByte('\n')
			last = i
			if out.Len() >= 8000 {
				_, err = io.WriteString(w, truncateBytes(out.String(), 8000))
				return err
			}
		}
	}
	_, err = w.Write(out.Bytes())
	return err
}

func previewLine(line string, at int) string {
	// A long context line must not consume the preview before the actual hit.
	const width = 1000
	if len(line) <= width {
		return line
	}
	start := min(max(0, at-200), len(line)-width)
	for start > 0 && !utf8.RuneStart(line[start]) {
		start--
	}
	text := truncateBytes(line[start:], width)
	if start+len(text) < len(line) {
		text += " ..."
	}
	if start > 0 {
		text = "... " + text
	}
	return text
}
