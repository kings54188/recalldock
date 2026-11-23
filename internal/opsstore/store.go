package opsstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"recalldock.local/recalldock/internal/worklog"
)

type Store struct {
	db              *sql.DB
	dir             string
	ID              string
	hasWriterColumn bool
	ownsDB          bool
	writer          *Writer
}

const schema = `
CREATE TABLE IF NOT EXISTS ops_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workspaces (id TEXT PRIMARY KEY, root TEXT NOT NULL UNIQUE, identity TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS tasks (
 workspace TEXT NOT NULL REFERENCES workspaces(id), lane TEXT NOT NULL,
 generation INTEGER NOT NULL, checkpoint_id INTEGER, goal TEXT NOT NULL,
 status TEXT NOT NULL, needs_check INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL,
 PRIMARY KEY(workspace,lane)
);
CREATE TABLE IF NOT EXISTS checkpoints (
 id INTEGER PRIMARY KEY AUTOINCREMENT, workspace TEXT NOT NULL, lane TEXT NOT NULL,
 generation INTEGER NOT NULL, goal TEXT NOT NULL, status TEXT NOT NULL, needs_check INTEGER NOT NULL,
 body TEXT NOT NULL, writer TEXT, created_at TEXT NOT NULL, UNIQUE(workspace,lane,generation),
 FOREIGN KEY(workspace,lane) REFERENCES tasks(workspace,lane)
);
CREATE TABLE IF NOT EXISTS operations (
 workspace TEXT NOT NULL, lane TEXT NOT NULL, id TEXT NOT NULL, fingerprint TEXT NOT NULL,
 status TEXT NOT NULL, evidence TEXT NOT NULL, PRIMARY KEY(workspace,lane,id),
 FOREIGN KEY(workspace,lane) REFERENCES tasks(workspace,lane)
);
CREATE TABLE IF NOT EXISTS events (
 workspace TEXT NOT NULL, lane TEXT NOT NULL, id TEXT NOT NULL, digest TEXT NOT NULL,
 generation INTEGER NOT NULL, body TEXT NOT NULL,
 PRIMARY KEY(workspace,lane,id,digest), FOREIGN KEY(workspace,lane,generation) REFERENCES checkpoints(workspace,lane,generation)
);
CREATE TABLE IF NOT EXISTS tool_catalog (
 server TEXT NOT NULL, name TEXT NOT NULL, config_hash TEXT NOT NULL, schema_hash TEXT NOT NULL,
 summary TEXT NOT NULL, schema_json TEXT NOT NULL, updated_at TEXT NOT NULL,
 PRIMARY KEY(server,name)
);
CREATE TABLE IF NOT EXISTS host_sessions (
 id TEXT PRIMARY KEY, host TEXT NOT NULL, root TEXT NOT NULL, identity TEXT NOT NULL,
 started_at TEXT NOT NULL, ended_at TEXT
);`

func Open(dir string, write bool) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("operations data directory must be absolute")
	}
	if write {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(dir, "operations.db")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && write {
		f, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr == nil {
			createErr = f.Close()
		}
		if createErr != nil && !errors.Is(createErr, os.ErrExist) {
			return nil, createErr
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("operations database must be a regular file, not a symlink")
	}
	mode := "ro"
	if write {
		mode = "rw"
	}
	uri := sqliteFileURI(path, "mode="+mode+"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir, ownsDB: true}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	var tables, metadata int
	if err := db.QueryRow("SELECT count(*), coalesce(sum(name='ops_metadata'),0) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables, &metadata); err != nil {
		return nil, err
	}
	if tables > 0 && metadata == 0 {
		return nil, errors.New("not an operations database; refusing to modify another store")
	}
	if metadata != 0 {
		if err := s.verify(); err != nil {
			return nil, err
		}
	} else if !write {
		return nil, errors.New("operations database is not initialized")
	}
	if write {
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			return nil, err
		}
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(schema); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO ops_metadata(key,value) VALUES ('kind','operations'),('schema_version','2'),('store_id',?)", "ops-"+randomID()); err != nil {
			return nil, err
		}
		if err := migrateWriterSchema(tx); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		if err := s.verify(); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func (s *Store) verify() error {
	var kind, version string
	if err := s.db.QueryRow("SELECT (SELECT value FROM ops_metadata WHERE key='kind'), (SELECT value FROM ops_metadata WHERE key='schema_version'), (SELECT value FROM ops_metadata WHERE key='store_id')").Scan(&kind, &version, &s.ID); err != nil {
		return err
	}
	if kind != "operations" || (version != "1" && version != "2") || !strings.HasPrefix(s.ID, "ops-") {
		return errors.New("unsupported operations database identity or schema")
	}
	var columns int
	if err := s.db.QueryRow("SELECT count(*) FROM pragma_table_info('checkpoints') WHERE name='writer'").Scan(&columns); err != nil {
		return err
	}
	s.hasWriterColumn = columns != 0
	if version == "2" && !s.hasWriterColumn {
		return errors.New("operations writer schema is incomplete")
	}
	return nil
}

func (s *Store) Close() error {
	if !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

type rootMarker struct {
	Version     int    `json:"version"`
	StoreID     string `json:"store_id"`
	WorkspaceID string `json:"workspace_id"`
	Root        string `json:"root"`
	Identity    string `json:"identity"`
}

type queryer interface{ QueryRow(string, ...any) *sql.Row }

func sqliteFileURI(path, rawQuery string) string {
	slashed := filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed, RawQuery: rawQuery}).String()
}

func sameOpsPath(a, b string) bool {
	ca, cb := expandOpsShortPath(a), expandOpsShortPath(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func normalizeOpsPath(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" && len(cleaned) >= 2 && cleaned[1] == ':' && cleaned[0] >= 'a' && cleaned[0] <= 'z' {
		cleaned = strings.ToUpper(cleaned[:1]) + cleaned[1:]
	}
	return cleaned
}

func canonicalRoot(root string) (string, string, error) {
	if !filepath.IsAbs(root) {
		return "", "", fmt.Errorf("root must be an absolute path: got %q", root)
	}
	if runtime.GOOS == "windows" && strings.HasPrefix(filepath.Clean(root), `\\`) {
		return "", "", fmt.Errorf("UNC network paths are not supported for operations workspace root: %q", root)
	}
	if len(root) > 1024 || !safeText(root, 1024) {
		return "", "", ErrIdentity
	}
	canonical, err := filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("root does not exist; select an existing absolute directory %s: %w", root, os.ErrNotExist)
	}
	if err != nil {
		return "", "", err
	}
	root = normalizeOpsPath(canonical)
	identity, err := directoryIdentity(root)
	return root, identity, err
}

func readMarker(root string) (rootMarker, error) {
	var marker rootMarker
	dir := filepath.Join(root, ".recalldock")
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return marker, ErrIdentity
	} else if err != nil && !os.IsNotExist(err) {
		return marker, err
	}
	path := filepath.Join(dir, "ops-root.json")
	info, err := os.Lstat(path)
	if err != nil {
		return marker, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return marker, ErrIdentity
	}
	f, err := os.Open(path)
	if err != nil {
		return marker, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 || json.Unmarshal(b, &marker) != nil || marker.Version != 1 {
		return marker, ErrIdentity
	}
	return marker, nil
}

func (s *Store) workspace(q queryer, root, identity string) (string, error) {
	var id, storedRoot, storedIdentity string
	err := q.QueryRow("SELECT id,root,identity FROM workspaces WHERE root=?", root).Scan(&id, &storedRoot, &storedIdentity)
	marker, markerErr := readMarker(root)
	if errors.Is(err, sql.ErrNoRows) && markerErr == nil && marker.WorkspaceID != "" && marker.StoreID == s.ID && marker.Identity == identity && sameOpsPath(marker.Root, root) {
		err = q.QueryRow("SELECT id,root,identity FROM workspaces WHERE id=?", marker.WorkspaceID).Scan(&id, &storedRoot, &storedIdentity)
		if err == nil && !sameOpsPath(storedRoot, root) {
			return "", ErrIdentity
		}
	}
	if errors.Is(err, sql.ErrNoRows) && os.IsNotExist(markerErr) {
		return "", ErrNotFound
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err != nil || markerErr != nil || storedIdentity != identity || marker.StoreID != s.ID || marker.WorkspaceID != id || !sameOpsPath(marker.Root, root) || marker.Identity != identity {
		return "", ErrIdentity
	}
	return id, nil
}

type workspaceMarker struct {
	root, identity, id string
	directoryInfo      os.FileInfo
	fileInfo           os.FileInfo
	body               []byte
}

func (marker *workspaceMarker) write(f *os.File, body []byte) error {
	n, err := f.Write(body)
	marker.body = append([]byte(nil), body[:n]...)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if info, statErr := f.Stat(); statErr == nil {
		marker.fileInfo = info
	}
	return errors.Join(err, f.Close())
}

func (marker *workspaceMarker) cleanup(conn *sql.Conn) error {
	var exists int
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM workspaces WHERE id=?", marker.id).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	identity, err := directoryIdentity(marker.root)
	if err != nil || identity != marker.identity {
		return nil
	}
	dir := filepath.Join(marker.root, ".recalldock")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !os.SameFile(info, marker.directoryInfo) {
		return nil
	}
	path := filepath.Join(dir, "ops-root.json")
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || marker.fileInfo == nil || !os.SameFile(info, marker.fileInfo) || info.Mode() != marker.fileInfo.Mode() || !info.ModTime().Equal(marker.fileInfo.ModTime()) {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(openedInfo, marker.fileInfo) {
		f.Close()
		return err
	}
	body, err := io.ReadAll(io.LimitReader(f, int64(len(marker.body)+1)))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if !bytes.Equal(body, marker.body) {
		return nil
	}
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, marker.fileInfo) || info.Mode() != marker.fileInfo.Mode() || !info.ModTime().Equal(marker.fileInfo.ModTime()) {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(dir)
}

func (s *Store) createWorkspace(tx *sql.Tx, root, identity string) (string, *workspaceMarker, error) {
	var other string
	err := tx.QueryRow("SELECT root FROM workspaces WHERE identity=?", identity).Scan(&other)
	if err == nil {
		return "", nil, ErrIdentity
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	id := "ws-" + randomID()
	dir := filepath.Join(root, ".recalldock")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", nil, ErrIdentity
	}
	body, _ := json.Marshal(rootMarker{1, s.ID, id, root, identity})
	f, err := os.OpenFile(filepath.Join(dir, "ops-root.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", nil, ErrIdentity
	}
	created := &workspaceMarker{root: root, identity: identity, id: id, directoryInfo: info}
	created.fileInfo, err = f.Stat()
	if err != nil {
		f.Close()
		return "", created, err
	}
	if err := created.write(f, body); err != nil {
		return "", created, err
	}
	if _, err := tx.Exec("INSERT INTO workspaces(id,root,identity) VALUES(?,?,?)", id, root, identity); err != nil {
		return "", created, err
	}
	return id, created, nil
}

func (s *Store) Save(root, lane string, expected int64, cp Checkpoint) (Snapshot, error) {
	return s.save(root, lane, expected, expected+1, cp)
}

func (s *Store) save(root, lane string, expected, generation int64, cp Checkpoint) (snapshot Snapshot, resultErr error) {
	if expected < 0 || generation <= expected {
		return Snapshot{}, ErrGeneration
	}
	if cp.Status == "" {
		cp.Status = "active"
	}
	root, identity, err := canonicalRoot(root)
	if err != nil {
		return Snapshot{}, err
	}
	if err := s.validateCheckpoint(root, lane, cp); err != nil {
		return Snapshot{}, err
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return Snapshot{}, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return Snapshot{}, err
	}
	var created *workspaceMarker
	commitFailed := false
	defer func() {
		_ = tx.Rollback()
		if commitFailed {
			// A rejected SQLite COMMIT can leave the connection inside its transaction.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		if created != nil {
			if err := created.cleanup(conn); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("workspace marker recovery failed: %w", err))
			}
		}
	}()
	if err := s.validateWriter(tx, root, identity); err != nil {
		return Snapshot{}, err
	}
	workspace, err := s.workspace(tx, root, identity)
	if errors.Is(err, ErrNotFound) && expected == 0 {
		workspace, created, err = s.createWorkspace(tx, root, identity)
	}
	if err != nil {
		return Snapshot{}, err
	}
	var current int64
	err = tx.QueryRow("SELECT generation FROM tasks WHERE workspace=? AND lane=?", workspace, lane).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, err
	}
	if current != expected {
		return Snapshot{}, &GenerationConflictError{Expected: expected, Current: current}
	}
	snap, err := s.persist(tx, workspace, root, lane, generation, cp)
	if err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		commitFailed = true
		return Snapshot{}, err
	}
	return snap, nil
}

func (s *Store) persist(tx *sql.Tx, workspace, root, lane string, generation int64, cp Checkpoint) (Snapshot, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snap := Snapshot{Root: root, Lane: lane, Generation: generation, UpdatedAt: now, Checkpoint: cp, Writer: s.checkpointWriter()}
	if _, err := tx.Exec("INSERT OR IGNORE INTO tasks(workspace,lane,generation,goal,status,updated_at) VALUES(?,?,0,?,?,?)", workspace, lane, cp.Goal, cp.Status, now); err != nil {
		return Snapshot{}, err
	}
	for _, op := range cp.Operations {
		var fingerprint string
		err := tx.QueryRow("SELECT fingerprint FROM operations WHERE workspace=? AND lane=? AND id=?", workspace, lane, op.ID).Scan(&fingerprint)
		if err == nil && fingerprint != op.InputFingerprint {
			return Snapshot{}, ErrOperationConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Snapshot{}, err
		}
		if _, err := tx.Exec(`INSERT INTO operations(workspace,lane,id,fingerprint,status,evidence) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(workspace,lane,id) DO UPDATE SET status=excluded.status,evidence=excluded.evidence`, workspace, lane, op.ID, op.InputFingerprint, op.Status, op.Evidence); err != nil {
			return Snapshot{}, err
		}
	}
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM operations WHERE workspace=? AND lane=? AND status IN ('started','applied','needs_check'))", workspace, lane).Scan(&snap.NeedsCheck); err != nil {
		return Snapshot{}, err
	}
	body, err := json.Marshal(cp)
	if err != nil {
		return Snapshot{}, err
	}
	writer, err := json.Marshal(snap.Writer)
	if err != nil {
		return Snapshot{}, err
	}
	result, err := tx.Exec("INSERT INTO checkpoints(workspace,lane,generation,goal,status,needs_check,body,writer,created_at) VALUES(?,?,?,?,?,?,?,?,?)", workspace, lane, generation, cp.Goal, cp.Status, snap.Task().NeedsCheck, string(body), string(writer), now)
	if err != nil {
		return Snapshot{}, err
	}
	snap.ID, err = result.LastInsertId()
	if err != nil {
		return Snapshot{}, err
	}
	_, err = tx.Exec("UPDATE tasks SET generation=?,checkpoint_id=?,goal=?,status=?,needs_check=?,updated_at=? WHERE workspace=? AND lane=?", generation, snap.ID, cp.Goal, cp.Status, snap.Task().NeedsCheck, now, workspace, lane)
	return snap, err
}

func (s *Store) readSnapshot(q queryer, workspace, root, lane string, generation int64) (Snapshot, error) {
	var body string
	var writer sql.NullString
	snap := Snapshot{Root: root, Lane: lane}
	err := q.QueryRow(`SELECT c.id,c.generation,c.body,c.created_at,`+s.writerSQL("c")+`,
	CASE WHEN ?=0 THEN EXISTS(SELECT 1 FROM operations o WHERE o.workspace=c.workspace AND o.lane=c.lane AND o.status IN ('started','applied','needs_check')) ELSE c.needs_check END FROM checkpoints c
	WHERE c.workspace=? AND c.lane=? AND c.generation=CASE WHEN ?=0 THEN
	(SELECT generation FROM tasks WHERE workspace=? AND lane=?) ELSE ? END`, generation, workspace, lane, generation, workspace, lane, generation).Scan(&snap.ID, &snap.Generation, &body, &snap.UpdatedAt, &writer, &snap.NeedsCheck)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	if err := json.Unmarshal([]byte(body), &snap.Checkpoint); err != nil {
		return Snapshot{}, err
	}
	snap.Writer, err = decodeWriter(writer)
	return snap, err
}

func (s *Store) Read(root, lane string, generation int64) (Snapshot, error) {
	if !namePattern.MatchString(lane) || generation < 0 {
		return Snapshot{}, ErrNotFound
	}
	root, identity, err := canonicalRoot(root)
	if err != nil {
		return Snapshot{}, err
	}
	workspace, err := s.workspace(s.db, root, identity)
	if err != nil {
		return Snapshot{}, err
	}
	return s.readSnapshot(s.db, workspace, root, lane, generation)
}

func (s *Store) Append(root, lane string, expected int64, event Event, patch Patch) (Snapshot, error) {
	if !namePattern.MatchString(lane) || expected < 1 || expected == 9223372036854775807 {
		return Snapshot{}, ErrGeneration
	}
	if err := validateEvent(event); err != nil {
		return Snapshot{}, err
	}
	root, identity, err := canonicalRoot(root)
	if err != nil {
		return Snapshot{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	if err := s.validateWriter(tx, root, identity); err != nil {
		return Snapshot{}, err
	}
	workspace, err := s.workspace(tx, root, identity)
	if err != nil {
		return Snapshot{}, err
	}
	body, _ := json.Marshal(struct {
		Event Event `json:"event"`
		Patch Patch `json:"patch"`
	}{event, patch})
	hash := digest(body)
	var replayGeneration int64
	err = tx.QueryRow("SELECT generation FROM events WHERE workspace=? AND lane=? AND id=? AND digest=?", workspace, lane, event.ID, hash).Scan(&replayGeneration)
	if err == nil {
		snap, err := s.readSnapshot(tx, workspace, root, lane, replayGeneration)
		snap.Replayed = true
		return snap, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, err
	}
	if _, err := Encode(json.RawMessage(body), "l2", DetailLimit); err != nil {
		return Snapshot{}, err
	}
	snap, err := s.readSnapshot(tx, workspace, root, lane, 0)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Generation != expected {
		return Snapshot{}, &GenerationConflictError{Expected: expected, Current: snap.Generation}
	}
	cp := snap.Checkpoint
	if patch.Summary != nil {
		cp.Summary = *patch.Summary
	}
	if patch.Next != nil {
		cp.Next = *patch.Next
	}
	if patch.Status != nil {
		cp.Status = *patch.Status
	}
	if patch.Blockers != nil {
		cp.Blockers = *patch.Blockers
	}
	op := worklog.Operation{ID: event.ID, InputFingerprint: event.InputFingerprint, Status: event.Status, Evidence: event.Evidence}
	found := false
	for i := range cp.Operations {
		if cp.Operations[i].ID == op.ID {
			cp.Operations[i] = op
			found = true
			break
		}
	}
	if !found {
		cp.Operations = append(cp.Operations, op)
		if len(cp.Operations) > 12 {
			cp.Operations = cp.Operations[1:]
		}
	}
	if err := s.validateCheckpoint(root, lane, cp); err != nil {
		return Snapshot{}, err
	}
	snap, err = s.persist(tx, workspace, root, lane, expected+1, cp)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := tx.Exec("INSERT INTO events(workspace,lane,id,digest,generation,body) VALUES(?,?,?,?,?,?)", workspace, lane, event.ID, hash, snap.Generation, string(body)); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func pageLimit(n int) int {
	if n <= 0 || n > 20 {
		return 5
	}
	return n
}

func ValidateFindOptions(opts FindOptions) error {
	if !safeText(opts.Query, 200) || opts.Limit < 0 || opts.Limit > 20 {
		return errors.New("invalid bounded task query or limit; limit must be 1..20")
	}
	switch opts.Status {
	case "", "active", "blocked", "completed", "archived", "all":
	default:
		return errors.New("status must be active, blocked, completed, archived or all")
	}
	switch opts.Order {
	case "", "recent":
		if opts.After != "" {
			id, err := strconv.ParseInt(strings.TrimPrefix(opts.After, "recent:"), 10, 64)
			if !strings.HasPrefix(opts.After, "recent:") || err != nil || id < 1 {
				return errors.New("invalid recent cursor; reuse work_find after with order=recent")
			}
		}
	case "lane":
		if opts.After != "" && !namePattern.MatchString(opts.After) {
			return errors.New("invalid lane cursor; reuse work_find after with order=lane")
		}
	default:
		return errors.New("order must be recent or lane")
	}
	return nil
}

func (s *Store) Find(root string, opts FindOptions) (TaskPage, error) {
	page := TaskPage{Tasks: []Task{}, Order: opts.Order}
	if page.Order == "" {
		page.Order = "recent"
	}
	if err := ValidateFindOptions(opts); err != nil {
		return page, err
	}
	root, identity, err := canonicalRoot(root)
	if err != nil {
		return page, err
	}
	workspace, err := s.workspace(s.db, root, identity)
	if errors.Is(err, ErrNotFound) {
		return page, nil
	}
	if err != nil {
		return page, err
	}
	order, boundary := "checkpoint_id DESC", "(?=0 OR checkpoint_id<?)"
	var cursor any = int64(0)
	if page.Order == "lane" {
		order, boundary, cursor = "lane", "lane>?", opts.After
	} else if opts.After != "" {
		cursor, _ = strconv.ParseInt(strings.TrimPrefix(opts.After, "recent:"), 10, 64)
	}
	cursorArgs := []any{cursor}
	if page.Order == "recent" {
		cursorArgs = append(cursorArgs, cursor)
	}
	query := `WITH needle AS (SELECT lower(?) AS value), matched AS (
	 SELECT t.checkpoint_id,t.lane,t.generation,t.goal,t.status,
	 EXISTS(SELECT 1 FROM operations o WHERE o.workspace=t.workspace AND o.lane=t.lane AND o.status IN ('started','applied','needs_check')) AS needs_check,t.updated_at,` + s.writerSQL("c") + ` AS writer
	 FROM tasks t JOIN checkpoints c ON c.id=t.checkpoint_id CROSS JOIN needle
	 WHERE t.workspace=? AND ((?='' AND t.status<>'archived') OR ?='all' OR t.status=?)
	 AND (needle.value='' OR instr(lower(t.goal || char(10) || t.lane || char(10) ||
	 coalesce(json_extract(c.body,'$.summary'),'') || char(10) || coalesce(json_extract(c.body,'$.details'),'') || char(10) ||
	 coalesce(json_extract(c.body,'$.next'),'') || char(10) || coalesce(json_extract(c.body,'$.blockers'),'')),needle.value)>0
	 OR EXISTS (SELECT 1 FROM events e WHERE e.workspace=t.workspace AND e.lane=t.lane AND
	 instr(lower(coalesce(json_extract(e.body,'$.event.summary'),'') || char(10) ||
	 coalesce(json_extract(e.body,'$.event.details'),'')),needle.value)>0))),
	 selected AS (SELECT * FROM matched WHERE ` + boundary + ` ORDER BY ` + order + ` LIMIT ?)
	 SELECT selected.*,total FROM (SELECT count(*) AS total FROM matched) LEFT JOIN selected ON 1=1
	 ORDER BY ` + order
	args := []any{opts.Query, workspace, opts.Status, opts.Status, opts.Status}
	args = append(args, cursorArgs...)
	args = append(args, pageLimit(opts.Limit)+1)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	page, err = scanTaskPage(rows, root, page)
	if err != nil {
		return page, err
	}
	if len(page.Tasks) > pageLimit(opts.Limit) {
		page.Tasks = page.Tasks[:pageLimit(opts.Limit)]
		page.HasMore = true
		page.After = taskCursor(page.Tasks[len(page.Tasks)-1], page.Order)
	}
	return page, nil
}

func taskCursor(task Task, order string) string {
	if order == "lane" {
		return task.Lane
	}
	return "recent:" + strconv.FormatInt(task.ID, 10)
}

func (s *Store) History(root, lane string, before int64, limit int) (TaskPage, error) {
	page := TaskPage{Tasks: []Task{}}
	if !namePattern.MatchString(lane) || before < 0 {
		return page, ErrNotFound
	}
	if limit < 0 || limit > 20 {
		return page, errors.New("limit must be 1..20")
	}
	root, identity, err := canonicalRoot(root)
	if err != nil {
		return page, err
	}
	workspace, err := s.workspace(s.db, root, identity)
	if err != nil {
		return page, err
	}
	rows, err := s.db.Query(`WITH matched AS (
	 SELECT id AS checkpoint_id,lane,generation,goal,status,needs_check,created_at AS updated_at,`+s.writerSQL("")+` AS writer FROM checkpoints
	 WHERE workspace=? AND lane=?), selected AS (
	 SELECT * FROM matched WHERE (?=0 OR generation<?) ORDER BY generation DESC LIMIT ?)
	 SELECT selected.*,total FROM (SELECT count(*) AS total FROM matched) LEFT JOIN selected ON 1=1 ORDER BY generation DESC`, workspace, lane, before, before, pageLimit(limit)+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	page, err = scanTaskPage(rows, root, page)
	if err != nil {
		return page, err
	}
	if page.Total == 0 {
		return page, ErrNotFound
	}
	if len(page.Tasks) > pageLimit(limit) {
		page.Tasks = page.Tasks[:pageLimit(limit)]
		page.HasMore = true
		page.Before = page.Tasks[len(page.Tasks)-1].Generation
	}
	return page, nil
}

func scanTaskPage(rows *sql.Rows, root string, page TaskPage) (TaskPage, error) {
	for rows.Next() {
		var id, generation sql.NullInt64
		var lane, goal, status, updatedAt, writerBody sql.NullString
		var needsCheck sql.NullBool
		if err := rows.Scan(&id, &lane, &generation, &goal, &status, &needsCheck, &updatedAt, &writerBody, &page.Total); err != nil {
			return page, err
		}
		if id.Valid {
			writer, err := decodeWriter(writerBody)
			if err != nil {
				return page, err
			}
			page.Tasks = append(page.Tasks, Task{Root: root, ID: id.Int64, Lane: lane.String, Generation: generation.Int64, Goal: clip(goal.String, 200), Status: status.String, NeedsCheck: needsCheck.Bool, UpdatedAt: updatedAt.String, Writer: writer})
		}
	}
	return page, rows.Err()
}

func (s *Store) Event(root, lane string, generation int64) (json.RawMessage, error) {
	snap, err := s.Read(root, lane, generation)
	if err != nil {
		return nil, err
	}
	var body string
	err = s.db.QueryRow(`SELECT e.body FROM events e JOIN workspaces w ON w.id=e.workspace WHERE w.root=? AND e.lane=? AND e.generation=?`, snap.Root, lane, snap.Generation).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return json.RawMessage(body), err
}

func (s *Store) Doctor() (map[string]any, error) {
	var integrity string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, err
	}
	if integrity != "ok" {
		return nil, fmt.Errorf("operations integrity check failed")
	}
	var tasks, checkpoints int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM checkpoints)").Scan(&tasks, &checkpoints); err != nil {
		return nil, err
	}
	return map[string]any{"kind": "operations", "store_id": s.ID, "integrity": integrity, "tasks": tasks, "checkpoints": checkpoints}, nil
}
