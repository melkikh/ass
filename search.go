package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Only required plain AND terms may narrow the candidate set. fzf remains the
// authority for exclusions and other extended syntax.
var simpleQuery = regexp.MustCompile(`^[a-zA-Z0-9_\- \p{Cyrillic}]+$`)

var sourceFilters = []string{"@codex", "@claude", "@cursor", "@opencode"}

// A backslash-space joins an fzf search term, so an @name after it is text.
var queryToken = regexp.MustCompile(`(?:\\ |[^\s])+`)

func splitSearchQuery(q string) (string, []string) {
	if !strings.Contains(q, "@") {
		return q, nil
	}
	var sources []string
	var text strings.Builder
	start := 0
	for _, span := range queryToken.FindAllStringIndex(q, -1) {
		token := strings.ToLower(q[span[0]:span[1]])
		if slices.Contains(sourceFilters, token) {
			source := token[1:]
			if !slices.Contains(sources, source) {
				sources = append(sources, source)
			}
			text.WriteString(q[start:span[0]])
			start = span[1]
		}
	}
	if len(sources) == 0 {
		return q, nil
	}
	text.WriteString(q[start:])
	return text.String(), sources
}

func candidateTerms(q string) []string {
	if !simpleQuery.MatchString(q) {
		// OR can make a positive term optional. Other unsupported positive
		// syntax also falls back to the full candidate set.
		if strings.Contains(q, "|") {
			return nil
		}
		var required []string
		for _, term := range queryToken.FindAllString(q, -1) {
			if strings.HasPrefix(term, "!") {
				continue
			}
			if !simpleQuery.MatchString(term) {
				return nil
			}
			required = append(required, term)
		}
		q = strings.Join(required, " ")
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
func (s *store) candidates(ctx context.Context, q string, all bool, sources []string) ([]session, error) {
	where := " WHERE id<>''"
	if !all {
		where += " AND titled=1 AND internal=0"
	}
	var args []any
	if len(sources) > 0 {
		where += " AND source IN (?" + strings.Repeat(",?", len(sources)-1) + ")"
		for _, source := range sources {
			args = append(args, source)
		}
	}
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
func (s *store) writeBody(ctx context.Context, v session, all bool, w io.Writer) error {
	query := "SELECT coalesce(flat,text),flat IS NULL FROM chunks WHERE session=? AND role<>'title'"
	if !all {
		query += " AND tool=0"
	}
	query += " ORDER BY key"
	rows, err := s.db.QueryContext(ctx, query, v.Key)
	if err != nil {
		return err
	}
	defer rows.Close()
	var text string
	var legacy bool
	for rows.Next() {
		if err = rows.Scan(&text, &legacy); err != nil {
			return err
		}
		// An older running picker can still append rows after the migration.
		if legacy {
			text = flatten(text)
		}
		if _, err = io.WriteString(w, " "); err != nil {
			return err
		}
		if _, err = io.WriteString(w, text); err != nil {
			return err
		}
	}
	return rows.Err()
}
func title(v session) string {
	if v.Title != "" {
		return v.Title
	}
	if v.Fallback != "" {
		return v.Fallback
	}
	if v.Snippet != "" {
		return truncate(v.Snippet, 120)
	}
	return "(empty)"
}
func sourceBadge(source, label string) string {
	badge := map[string]string{"codex": "\x1b[36m◆", "claude": "\x1b[33m◈", "cursor": "\x1b[35m▣", "opencode": "\x1b[32m◉"}[source]
	if badge == "" {
		return ""
	}
	return badge + label + "\x1b[0m"
}
func display(v session) string {
	badge := sourceBadge(v.Source, "")
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
	q, sources := splitSearchQuery(q)
	candidates, err := s.candidates(ctx, q, all, sources)
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
	// Keep matcher input plain; only the displayed results need ANSI styling.
	cmd := exec.CommandContext(ctx, "fzf", "--exact", "--delimiter=\t", "--with-nth=2,3,4", "--nth=2,3", "--accept-nth=1", "--filter="+q)
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
		bw := bufio.NewWriterSize(writer, 64<<10)
		for _, v := range candidates {
			name := flatten(title(v))
			// Negation and end anchors can stop matching when tools are added.
			// Match both bodies so -a always includes ordinary search results.
			for _, mode := range []bool{false, true} {
				if mode && (!all || v.AllBytes == v.PlainBytes) {
					break
				}
				if _, e = fmt.Fprintf(bw, "%d\t◆\t%s\t", v.Key, name); e != nil {
					return
				}
				if e = s.writeBody(ctx, v, mode, bw); e != nil {
					return
				}
				if e = bw.WriteByte('\n'); e != nil {
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
	if source == "" || id == "" {
		return nil
	}
	var key int64
	if err := s.db.QueryRowContext(ctx, "SELECT key FROM sessions WHERE source=? AND id=? ORDER BY mtime DESC LIMIT 1", source, id).Scan(&key); err != nil {
		// fzf also requests previews with no selection or while a refresh removes it.
		if err == sql.ErrNoRows {
			return nil
		}
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
	q, _ = splitSearchQuery(q)
	var terms []string
	for _, term := range queryToken.FindAllString(q, -1) {
		// Exclusions select chats, not lines to show inside those chats.
		if term == "|" || strings.HasPrefix(term, "!") {
			continue
		}
		term = strings.ToLower(strings.Trim(strings.ReplaceAll(term, `\ `, " "), "'^$"))
		if term != "" {
			terms = append(terms, term)
		}
	}
	showAll := len(terms) == 0
	type previewRow struct {
		speaker, text string
		newChunk      bool
	}
	var lines []previewRow
	var selected []int
	var total int
	for rows.Next() {
		var role, text string
		var tool bool
		if err = rows.Scan(&role, &text, &tool); err != nil {
			return err
		}
		speaker := previewSpeaker(source, role, tool)
		// Older caches may retain carriage returns and C1 terminal controls.
		for i, line := range strings.Split(cleanControls(text), "\n") {
			match := showAll
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
			lines = append(lines, previewRow{speaker, line, i == 0})
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
	var speaker string
	for _, hit := range selected {
		start := max(0, hit-3)
		end := min(len(lines), hit+4)
		for i := max(start, last+1); i < end; i++ {
			row := lines[i]
			var heading string
			switch {
			case i > last+1:
				if last >= 0 {
					heading = "\n"
				}
				heading += "\x1b[2m…\x1b[0m\n\n"
				speaker = ""
			case last >= 0 && (row.newChunk || row.speaker != speaker) && lines[last].text != "":
				heading = "\n"
			}
			if row.speaker != speaker {
				heading += row.speaker + "\n"
			}
			if out.Len()+len(heading)+len(row.text)+3 > 8000 {
				_, err = out.WriteTo(w)
				return err
			}
			out.WriteString(heading)
			if row.text != "" {
				out.WriteString("  ")
				out.WriteString(row.text)
			}
			out.WriteByte('\n')
			last = i
			speaker = row.speaker
		}
	}
	_, err = out.WriteTo(w)
	return err
}

func previewSpeaker(source, role string, tool bool) string {
	switch {
	case tool:
		return "\x1b[2m· TOOL\x1b[0m"
	case role == "user":
		return "\x1b[90m● YOU\x1b[0m"
	}
	if badge := sourceBadge(source, " "+strings.ToUpper(source)); badge != "" {
		return badge
	}
	return "\x1b[1;35m◆ AGENT\x1b[0m"
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
