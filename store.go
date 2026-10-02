package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 3

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type store struct {
	db  *sql.DB
	cfg config
}
type session struct {
	Key                                                                                 int64
	Source, Path, ID, Native, CWD, Title, Fallback, Snippet, Fingerprint, Inode, Anchor string
	Titled, Internal                                                                    bool
	Mtime, Size, Offset, PlainBytes, AllBytes                                           int64
}

const sessionFields = `key,source,path,id,native,cwd,title,fallback,snippet,fingerprint,inode,anchor,titled,internal,mtime,size,offset,plain_bytes,all_bytes`

func readSession(row interface{ Scan(...any) error }) (s session, err error) {
	err = row.Scan(&s.Key, &s.Source, &s.Path, &s.ID, &s.Native, &s.CWD, &s.Title, &s.Fallback, &s.Snippet, &s.Fingerprint, &s.Inode, &s.Anchor, &s.Titled, &s.Internal, &s.Mtime, &s.Size, &s.Offset, &s.PlainBytes, &s.AllBytes)
	return
}
func privateDir(p string) error {
	if err := os.MkdirAll(p, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("not a private directory: %s", p)
	}
	return os.Chmod(p, 0700)
}
func openStore(c config) (*store, error) {
	if err := privateDir(c.Cache); err != nil {
		return nil, err
	}
	p := filepath.Join(c.Cache, indexName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(p, 0600); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: p}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(2)&_pragma=secure_delete(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &store{db, c}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version < 0 || version > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("unsupported ass index version %d", version)
	}
	if version == 0 {
		_, err = db.Exec(`PRAGMA journal_mode=WAL;
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS sessions (
 key INTEGER PRIMARY KEY, source TEXT NOT NULL, path TEXT NOT NULL, id TEXT NOT NULL DEFAULT '', native TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '',
 title TEXT NOT NULL DEFAULT '', fallback TEXT NOT NULL DEFAULT '', snippet TEXT NOT NULL DEFAULT '', fingerprint TEXT NOT NULL DEFAULT '', inode TEXT NOT NULL DEFAULT '', anchor TEXT NOT NULL DEFAULT '',
 titled INTEGER NOT NULL DEFAULT 0, internal INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL DEFAULT 0, size INTEGER NOT NULL DEFAULT 0, offset INTEGER NOT NULL DEFAULT 0,
 plain_bytes INTEGER NOT NULL DEFAULT 0, all_bytes INTEGER NOT NULL DEFAULT 0, UNIQUE(source,path));
CREATE TABLE IF NOT EXISTS chunks (key INTEGER PRIMARY KEY, session INTEGER NOT NULL REFERENCES sessions(key) ON DELETE CASCADE, role TEXT NOT NULL, tool INTEGER NOT NULL, all_start INTEGER NOT NULL, text TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS chunk_session ON chunks(session,key);
CREATE VIRTUAL TABLE IF NOT EXISTS terms USING fts5(body,content='',contentless_delete=1,detail=none,tokenize='trigram remove_diacritics 1');
CREATE TRIGGER IF NOT EXISTS chunk_delete AFTER DELETE ON chunks BEGIN DELETE FROM terms WHERE rowid=old.key; END;
CREATE TABLE IF NOT EXISTS sources (source TEXT PRIMARY KEY, fingerprint TEXT NOT NULL);
PRAGMA user_version=3;
COMMIT;`)
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	if version > 0 && version < schemaVersion {
		if err = rebuildIndex(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func rebuildIndex(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	var version int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	// The parser changed: old offsets and fingerprints would preserve omitted
	// tools and unwanted metadata. Another process may have rebuilt it already.
	if version == 1 {
		if _, err = conn.ExecContext(ctx, "DELETE FROM sessions; DELETE FROM sources"); err != nil {
			return err
		}
	}
	if version == 2 {
		if _, err = conn.ExecContext(ctx, "DELETE FROM sessions WHERE source='codex'"); err != nil {
			return err
		}
	}
	if version < schemaVersion {
		if _, err = conn.ExecContext(ctx, "PRAGMA user_version=3"); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func (s *store) existing(ctx context.Context, source string) (map[string]session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+sessionFields+" FROM sessions WHERE source=?", source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]session{}
	for rows.Next() {
		v, e := readSession(rows)
		if e != nil {
			return nil, e
		}
		out[v.Path] = v
	}
	return out, rows.Err()
}
func saveSession(ctx context.Context, tx *sql.Tx, s *session) error {
	if s.Key == 0 {
		r, err := tx.ExecContext(ctx, "INSERT INTO sessions(source,path) VALUES(?,?)", s.Source, s.Path)
		if err != nil {
			return err
		}
		s.Key, err = r.LastInsertId()
		if err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET id=?,native=?,cwd=?,title=?,fallback=?,snippet=?,fingerprint=?,inode=?,anchor=?,titled=?,internal=?,mtime=?,size=?,offset=?,plain_bytes=?,all_bytes=? WHERE key=?`, s.ID, s.Native, s.CWD, s.Title, s.Fallback, s.Snippet, s.Fingerprint, s.Inode, s.Anchor, s.Titled, s.Internal, s.Mtime, s.Size, s.Offset, s.PlainBytes, s.AllBytes, s.Key)
	return err
}
func resetSession(ctx context.Context, tx *sql.Tx, s *session) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE session=?", s.Key)
	s.Title = ""
	s.Fallback = ""
	s.Snippet = ""
	s.Titled = false
	s.Internal = false
	s.Offset = 0
	s.PlainBytes = 0
	s.AllBytes = 0
	s.CWD = ""
	return err
}
func putChunk(ctx context.Context, tx *sql.Tx, s *session, role, text string, tool bool) error {
	if role == "user" && !tool {
		text = cleanUser(text)
	}
	text = cleanControls(text)
	if tool {
		text = limitTool(text)
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	flat := flatten(text)
	r, err := tx.ExecContext(ctx, "INSERT INTO chunks(session,role,tool,all_start,text) VALUES(?,?,?,?,?)", s.Key, role, tool, s.AllBytes, text)
	if err != nil {
		return err
	}
	k, err := r.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO terms(rowid,body) VALUES(?,?)", k, flat); err != nil {
		return err
	}
	if role == "title" {
		return nil
	}
	if s.Fallback == "" && !tool {
		s.Fallback = truncate(flat, 120)
	}
	if !tool {
		s.PlainBytes += int64(len(flat) + 1)
		if s.Snippet == "" {
			s.Snippet = truncate(flat, 180)
		}
	}
	s.AllBytes += int64(len(flat) + 1)
	return nil
}
func finishTitle(ctx context.Context, tx *sql.Tx, s *session) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE session=? AND role='title'", s.Key)
	if err != nil {
		return err
	}
	t := s.Title
	if !s.Titled {
		t = s.Fallback
	}
	if t == "" {
		t = "(empty)"
	}
	return putChunk(ctx, tx, s, "title", t, false)
}
func (s *store) removeMissing(ctx context.Context, source string, old map[string]session) (int, error) {
	if len(old) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, v := range old {
		if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE key=?", v.Key); err != nil {
			return 0, err
		}
	}
	return len(old), tx.Commit()
}
func sourceFingerprint(path string) string {
	var b strings.Builder
	for _, p := range []string{path, path + "-wal"} {
		info, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(&b, "%s:missing;", p)
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d:%s;", p, info.Size(), info.ModTime().UnixNano(), inode(info))
	}
	return b.String()
}
func inode(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	return ""
}

func (s *store) update(ctx context.Context) (int, error) {
	f, err := os.OpenFile(filepath.Join(s.cfg.Cache, lockName), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	total := 0
	var errs []string
	for _, source := range []string{"codex", "claude", "cursor", "opencode"} {
		var n int
		var e error
		switch source {
		case "codex":
			n, e = s.updateFiles(ctx, source, s.cfg.Codex)
		case "claude":
			n, e = s.updateFiles(ctx, source, s.cfg.Claude)
		case "cursor":
			n, e = s.updateCursor(ctx)
		case "opencode":
			n, e = s.updateOpenCode(ctx)
		}
		total += n
		if e != nil {
			errs = append(errs, source+": "+e.Error())
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
	if len(errs) > 0 {
		return total, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return total, nil
}
