package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type sourceDB struct {
	db                *sql.DB
	tx                *sql.Tx
	path, fingerprint string
}

func openSource(ctx context.Context, path string) (*sourceDB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	// Live sources need SQLite's locks even when no WAL file exists yet.
	q.Add("_pragma", "busy_timeout(3000)")
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// A first read may create WAL coordination files. Settle those before taking
	// the fingerprint, but capture it before the transaction acquires its snapshot.
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	fingerprint := sourceFingerprint(path)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &sourceDB{db, tx, path, fingerprint}, nil
}
func (d *sourceDB) close() { d.tx.Rollback(); d.db.Close() }
func (s *store) databaseState(ctx context.Context, source, path string) (map[string]session, bool, error) {
	old, err := s.existing(ctx, source)
	if err != nil {
		return nil, false, err
	}
	if _, err = os.Stat(path); os.IsNotExist(err) {
		_, err = s.removeMissing(ctx, source, old)
		if err == nil {
			_, err = s.db.ExecContext(ctx, "DELETE FROM sources WHERE source=?", source)
		}
		return nil, true, err
	} else if err != nil {
		return nil, false, err
	}
	var previous string
	err = s.db.QueryRowContext(ctx, "SELECT fingerprint FROM sources WHERE source=?", source).Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return nil, false, err
	}
	return old, previous == sourceFingerprint(path), nil
}
func (s *store) finishSource(ctx context.Context, source string, d *sourceDB, old map[string]session) (int, error) {
	// Never bless a newer source snapshot than the one actually read.
	if sourceFingerprint(d.path) != d.fingerprint {
		return 0, fmt.Errorf("history changed during scan; will retry")
	}
	n, err := s.removeMissing(ctx, source, old)
	if err != nil {
		return n, err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO sources(source,fingerprint) VALUES(?,?) ON CONFLICT(source) DO UPDATE SET fingerprint=excluded.fingerprint", source, d.fingerprint)
	return n, err
}

func syntheticID(native string) string {
	h := digest([]byte(native))
	return h[:8] + "-" + h[8:12] + "-5" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

type record struct {
	Role, Text string
	Tool       bool
	Group      string
}

type transcript struct {
	ID, Native, Title, CWD string
	Updated                int64
	Records                []record
}

func (s *store) replaceTranscript(ctx context.Context, v *session, t transcript) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveSession(ctx, tx, v); err != nil {
		return err
	}
	if err = resetSession(ctx, tx, v); err != nil {
		return err
	}
	v.ID = t.ID
	v.Native = t.Native
	v.Title = truncate(flatten(t.Title), 120)
	v.Titled = strings.TrimSpace(t.Title) != ""
	v.CWD = t.CWD
	v.Mtime = t.Updated * 1000000
	for _, r := range t.Records {
		if err = putChunk(ctx, tx, v, r.Role, r.Text, r.Tool); err != nil {
			return err
		}
	}
	if err = finishTitle(ctx, tx, v); err != nil {
		return err
	}
	if err = saveSession(ctx, tx, v); err != nil {
		return err
	}
	return tx.Commit()
}

const openCodeMetadata = `SELECT s.id,s.title,s.directory,s.time_updated,
 coalesce((SELECT json_array(count(*),max(time_updated),sum(length(data))) FROM message WHERE session_id=s.id),'[]'),
 coalesce((SELECT json_array(count(*),max(time_updated),sum(length(data))) FROM part WHERE session_id=s.id),'[]')
 FROM session s WHERE s.parent_id IS NULL ORDER BY s.time_updated DESC,s.id`

func (s *store) updateOpenCode(ctx context.Context) (int, error) {
	old, skip, err := s.databaseState(ctx, "opencode", s.cfg.OpenCode)
	if err != nil || skip {
		return 0, err
	}
	d, err := openSource(ctx, s.cfg.OpenCode)
	if err != nil {
		return 0, err
	}
	defer d.close()
	rows, err := d.tx.QueryContext(ctx, openCodeMetadata)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		t           transcript
		fingerprint string
	}
	var candidates []candidate
	for rows.Next() {
		var t transcript
		var m, p string
		if err = rows.Scan(&t.Native, &t.Title, &t.CWD, &t.Updated, &m, &p); err != nil {
			rows.Close()
			return 0, err
		}
		if !nativeRE.MatchString(t.Native) {
			continue
		}
		t.ID = syntheticID(t.Native)
		b, _ := json.Marshal([]any{t.Native, t.Title, t.CWD, t.Updated, m, p})
		candidates = append(candidates, candidate{t, digest(b)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, c := range candidates {
		path := s.cfg.OpenCode + "#" + c.t.Native
		v, exists := old[path]
		delete(old, path)
		if exists && v.Fingerprint == c.fingerprint {
			continue
		}
		if !exists {
			v = session{Source: "opencode", Path: path}
		}
		v.Fingerprint = c.fingerprint
		c.t.Records, err = openCodeRecords(ctx, d.tx, c.t.Native)
		if err != nil {
			return changed, err
		}
		if err = s.replaceTranscript(ctx, &v, c.t); err != nil {
			return changed, err
		}
		changed++
	}
	n, err := s.finishSource(ctx, "opencode", d, old)
	return changed + n, err
}
func openCodeRecords(ctx context.Context, tx *sql.Tx, id string) ([]record, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id,json_extract(m.data,'$.role'),p.data
 FROM message m JOIN part p ON p.message_id=m.id WHERE m.session_id=? AND json_valid(m.data) AND json_valid(p.data)
 AND json_extract(m.data,'$.role') IN ('user','assistant') ORDER BY m.time_created,m.id,p.time_created,p.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []record
	for rows.Next() {
		var group, role, data string
		if err = rows.Scan(&group, &role, &data); err != nil {
			return nil, err
		}
		var p object
		if err = json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		switch str(p["type"]) {
		case "text":
			if p["synthetic"] == true || p["ignored"] == true {
				continue
			}
			t := str(p["text"])
			records = append(records, record{role, t, false, group})
		case "tool":
			state := obj(p["state"])
			for _, text := range []string{toolCall(str(p["tool"]), state["input"]), toolText(state["output"]), toolText(state["error"])} {
				if strings.TrimSpace(text) != "" {
					records = append(records, record{"tool", text, true, group})
				}
			}
		}
	}
	return records, rows.Err()
}

// Cursor has no reliable updated_at on bubble rows. Hash the searchable fields
// when its database changes, then replace only changed conversations. Unlike
// OpenCode, arbitrary in-place Cursor edits require inspecting those fields.
func cursorQuery(header bool) string {
	join := ""
	meta := "'{}'"
	if header {
		join = `LEFT JOIN composerHeaders meta ON meta.composerId=json_extract(c.value,'$.composerId')`
		meta = `CASE WHEN json_valid(meta.value) THEN meta.value ELSE '{}' END`
	}
	bubble := `coalesce(b.value,json_extract(c.value,'$.conversationMap.'||json_quote(json_extract(h.value,'$.bubbleId'))),h.value)`
	return `SELECT json_object('id',json_extract(c.value,'$.composerId'),
 'updated',coalesce(json_extract(c.value,'$.lastUpdatedAt'),json_extract(c.value,'$.createdAt'),0),
 'title',coalesce(json_extract(c.value,'$.name'),json_extract(` + meta + `,'$.name')),
 'workspace',json(coalesce(json_extract(c.value,'$.workspaceIdentifier.uri'),json_extract(` + meta + `,'$.workspaceIdentifier.uri'),'{}')),
 'bubbles',json((SELECT json_group_array(json(frame)) FROM (SELECT json_object(
 'type',json_extract(` + bubble + `,'$.type'),
 'text',json_extract(` + bubble + `,'$.text'),
 'toolFormerData',json_extract(` + bubble + `,'$.toolFormerData'),
 'toolResults',json_extract(` + bubble + `,'$.toolResults')) AS frame
 FROM json_each(CASE WHEN json_array_length(json_extract(c.value,'$.fullConversationHeadersOnly'))>0 THEN json_extract(c.value,'$.fullConversationHeadersOnly') ELSE coalesce(json_extract(c.value,'$.conversation'),'[]') END) h
 LEFT JOIN cursorDiskKV b ON b.key='bubbleId:'||json_extract(c.value,'$.composerId')||':'||json_extract(h.value,'$.bubbleId') ORDER BY CAST(h.key AS INTEGER)))))
 FROM cursorDiskKV c ` + join + ` WHERE c.key LIKE 'composerData:%' AND c.value IS NOT NULL AND json_valid(c.value)`
}
func hasHeaders(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='composerHeaders'").Scan(&n)
	return n > 0, err
}
func decodeCursor(data string) (transcript, error) {
	var v struct {
		ID, Title string
		Updated   int64
		Workspace struct{ Scheme, FsPath, Path string }
		Bubbles   []struct {
			Type                        int
			Text                        string
			ToolFormerData, ToolResults any
		}
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return transcript{}, err
	}
	t := transcript{ID: v.ID, Title: v.Title, Updated: v.Updated}
	if v.Workspace.Scheme == "file" {
		t.CWD = v.Workspace.FsPath
		if t.CWD == "" {
			t.CWD = v.Workspace.Path
		}
	}
	for i, b := range v.Bubbles {
		group := fmt.Sprint(i)
		if b.Type != 1 && b.Type != 2 {
			continue
		}
		role := "assistant"
		if b.Type == 1 {
			role = "user"
			b.Text = cleanUser(b.Text)
			if t.Title == "" && strings.TrimSpace(b.Text) != "" {
				t.Title = b.Text
			}
		}
		if strings.TrimSpace(b.Text) != "" {
			t.Records = append(t.Records, record{role, b.Text, false, group})
		}
		t.Records = append(t.Records, cursorTools(b.ToolFormerData, group)...)
		t.Records = append(t.Records, cursorTools(b.ToolResults, group)...)
	}
	return t, nil
}
func (s *store) updateCursor(ctx context.Context) (int, error) {
	old, skip, err := s.databaseState(ctx, "cursor", s.cfg.Cursor)
	if err != nil || skip {
		return 0, err
	}
	d, err := openSource(ctx, s.cfg.Cursor)
	if err != nil {
		return 0, err
	}
	defer d.close()
	header, err := hasHeaders(ctx, d.tx)
	if err != nil {
		return 0, err
	}
	rows, err := d.tx.QueryContext(ctx, cursorQuery(header))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	changed := 0
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			return changed, err
		}
		t, e := decodeCursor(data)
		if e != nil {
			return changed, e
		}
		if !uuidRE.MatchString(t.ID) || len(t.Records) == 0 {
			continue
		}
		path := s.cfg.Cursor + "#" + t.ID
		v, exists := old[path]
		delete(old, path)
		fingerprint := digest([]byte(data))
		if exists && v.Fingerprint == fingerprint {
			continue
		}
		if !exists {
			v = session{Source: "cursor", Path: path}
		}
		v.Fingerprint = fingerprint
		if err = s.replaceTranscript(ctx, &v, t); err != nil {
			return changed, err
		}
		changed++
	}
	if err = rows.Err(); err != nil {
		return changed, err
	}
	rows.Close()
	n, err := s.finishSource(ctx, "cursor", d, old)
	return changed + n, err
}

func (s *store) export(ctx context.Context, source, id, dest string) error {
	if !uuidRE.MatchString(id) {
		return fmt.Errorf("invalid session ID")
	}
	v, err := readSession(s.db.QueryRowContext(ctx, "SELECT "+sessionFields+" FROM sessions WHERE source=? AND id=?", source, id))
	if err != nil {
		return err
	}
	var t transcript
	switch source {
	case "opencode":
		d, e := openSource(ctx, s.cfg.OpenCode)
		if e != nil {
			return e
		}
		defer d.close()
		t = transcript{ID: id, Native: v.Native}
		if err = d.tx.QueryRowContext(ctx, "SELECT title,directory,time_updated FROM session WHERE id=? AND parent_id IS NULL", v.Native).Scan(&t.Title, &t.CWD, &t.Updated); err != nil {
			return err
		}
		t.Records, err = openCodeRecords(ctx, d.tx, v.Native)
	case "cursor":
		d, e := openSource(ctx, s.cfg.Cursor)
		if e != nil {
			return e
		}
		defer d.close()
		header, e := hasHeaders(ctx, d.tx)
		if e != nil {
			return e
		}
		var data string
		err = d.tx.QueryRowContext(ctx, cursorQuery(header)+" AND json_extract(c.value,'$.composerId')=?", id).Scan(&data)
		if err == nil {
			t, err = decodeCursor(data)
		}
	default:
		return fmt.Errorf("cannot export source %q", source)
	}
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err = enc.Encode(object{"type": source + "-meta", "cwd": t.CWD, "nativeSessionId": t.Native, "sessionId": t.ID}); err != nil {
		return err
	}
	if t.Title != "" {
		if err = enc.Encode(object{"type": "ai-title", "aiTitle": t.Title}); err != nil {
			return err
		}
	}
	var group, role string
	var content []any
	flush := func() error {
		if len(content) == 0 {
			return nil
		}
		err := enc.Encode(object{"type": role, "message": object{"content": content}})
		content = nil
		return err
	}
	for _, r := range t.Records {
		if r.Tool || strings.TrimSpace(r.Text) == "" {
			continue
		}
		if r.Group != group || r.Role != role {
			if err = flush(); err != nil {
				return err
			}
			group, role = r.Group, r.Role
		}
		content = append(content, object{"type": "text", "text": r.Text})
	}
	if err = flush(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if t.Updated > 0 {
		ts := unixMillis(t.Updated)
		return os.Chtimes(dest, ts, ts)
	}
	return nil
}
