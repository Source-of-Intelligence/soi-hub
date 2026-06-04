package skillmarket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// SQLite schema
// =============================================================================

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS market_sources (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    UNIQUE NOT NULL,
    url        TEXT    NOT NULL,
    type       TEXT    DEFAULT 'http',
    enabled    INTEGER DEFAULT 1,
    priority   INTEGER DEFAULT 5,
    created_at TEXT    DEFAULT (datetime('now')),
    updated_at TEXT    DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS installed_skills (
    name        TEXT PRIMARY KEY,
    version     TEXT    DEFAULT '1.0.0',
    description TEXT    DEFAULT '',
    author      TEXT    DEFAULT '',
    runtime     TEXT    DEFAULT 'builtin',
    tags        TEXT    DEFAULT '[]',
    tools       TEXT    DEFAULT '[]',
    source_url  TEXT    DEFAULT '',
    source_type TEXT    DEFAULT 'local',
    has_wasm    INTEGER DEFAULT 0,
    has_soi     INTEGER DEFAULT 0,
    size_bytes  INTEGER DEFAULT 0,
    checksum    TEXT    DEFAULT '',
    status      TEXT    DEFAULT 'active',
    install_time TEXT   DEFAULT (datetime('now')),
    updated_at  TEXT    DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS install_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    skill_name    TEXT    NOT NULL,
    skill_version TEXT    DEFAULT '',
    source_url    TEXT    NOT NULL,
    source_type   TEXT    DEFAULT 'url',
    status        TEXT    DEFAULT 'pending',
    error_message TEXT    DEFAULT '',
    checksum      TEXT    DEFAULT '',
    file_size     INTEGER DEFAULT 0,
    started_at    TEXT    DEFAULT (datetime('now')),
    completed_at  TEXT
);

CREATE INDEX IF NOT EXISTS idx_ih_skill  ON install_history(skill_name);
CREATE INDEX IF NOT EXISTS idx_ih_status ON install_history(status);
`

// =============================================================================
// sqliteStore
// =============================================================================

type sqliteStore struct {
	db *sql.DB
	mu sync.RWMutex
}

func newSQLiteStore(dsn string) (*sqliteStore, error) {
	// For file paths, ensure the parent directory exists
	if !strings.HasPrefix(dsn, "file:") {
		dir := filepath.Dir(dsn)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("mkdir db dir: %w", err)
		}
	}

	dbDSN := dsn
	if !strings.HasPrefix(dsn, "file:") && !strings.Contains(dsn, "?") {
		dbDSN = dsn + "?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL"
	}

	db, err := sql.Open("sqlite", dbDSN)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	store := &sqliteStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	return store, nil
}

func (s *sqliteStore) migrate() error {
	_, err := s.db.Exec(sqliteSchema)
	return err
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// ── Sources ────────────────────────────────────────────────────────────────

func (s *sqliteStore) ListSources(ctx context.Context) ([]*MarketSource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, url, type, enabled, priority, created_at, updated_at
		 FROM market_sources ORDER BY priority DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*MarketSource
	for rows.Next() {
		var m MarketSource
		var enabled int
		var ca, ua string
		if err := rows.Scan(&m.ID, &m.Name, &m.URL, &m.Type, &enabled, &m.Priority, &ca, &ua); err != nil {
			return nil, err
		}
		m.Enabled = enabled != 0
		m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ca)
		m.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", ua)
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (s *sqliteStore) AddSource(ctx context.Context, src *MarketSource) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	enabled := 0
	if src.Enabled {
		enabled = 1
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO market_sources (name, url, type, enabled, priority, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		src.Name, src.URL, src.Type, enabled, src.Priority, now)
	return err
}

func (s *sqliteStore) RemoveSource(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM market_sources WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("source not found: %s", name)
	}
	return nil
}

// ── Installed skills ───────────────────────────────────────────────────────

func (s *sqliteStore) ListSkills(ctx context.Context) ([]*InstalledSkill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, selectSkillCols+` FROM installed_skills ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSkills(rows)
}

func (s *sqliteStore) GetSkill(ctx context.Context, name string) (*InstalledSkill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx, selectSkillCols+` FROM installed_skills WHERE name = ?`, name)
	return scanOneSkill(row)
}

func (s *sqliteStore) UpsertSkill(ctx context.Context, sk *InstalledSkill) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sk.Status == "" {
		sk.Status = "active"
	}
	if sk.Runtime == "" {
		sk.Runtime = "builtin"
	}
	if sk.SourceType == "" {
		sk.SourceType = "local"
	}

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	tagsJSON, _ := json.Marshal(coalesceStrSlice(sk.Tags))
	toolsJSON, _ := json.Marshal(coalesceStrSlice(sk.Tools))
	hasWasm, hasSoi := boolToInt(sk.HasWasm), boolToInt(sk.HasSoi)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO installed_skills
			(name, version, description, author, runtime, tags, tools,
			 source_url, source_type, has_wasm, has_soi, size_bytes, checksum, status, install_time, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
			version=excluded.version, description=excluded.description, author=excluded.author,
			runtime=excluded.runtime, tags=excluded.tags, tools=excluded.tools,
			source_url=excluded.source_url, source_type=excluded.source_type,
			has_wasm=excluded.has_wasm, has_soi=excluded.has_soi,
			size_bytes=excluded.size_bytes, checksum=excluded.checksum,
			status=excluded.status, updated_at=excluded.updated_at`,
		sk.Name, sk.Version, sk.Description, sk.Author, sk.Runtime,
		string(tagsJSON), string(toolsJSON),
		sk.SourceURL, sk.SourceType, hasWasm, hasSoi, sk.SizeBytes, sk.Checksum,
		sk.Status, sk.InstallTime.Format("2006-01-02 15:04:05"), now)
	return err
}

func (s *sqliteStore) DeleteSkill(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM installed_skills WHERE name = ?`, name)
	return err
}

func (s *sqliteStore) SearchSkills(ctx context.Context, query string) ([]*InstalledSkill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pattern := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx,
		selectSkillCols+` FROM installed_skills
		 WHERE name LIKE ? OR description LIKE ? OR author LIKE ?
		 ORDER BY name ASC`,
		pattern, pattern, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSkills(rows)
}

// ── Install history ────────────────────────────────────────────────────────

func (s *sqliteStore) AddInstallHistory(ctx context.Context, h *InstallHistory) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO install_history (skill_name, skill_version, source_url, source_type, checksum, started_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		h.SkillName, h.SkillVersion, h.SourceURL, h.SourceType, h.Checksum, now)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	h.ID = int(id)
	h.StartedAt = time.Now().UTC()
	return nil
}

func (s *sqliteStore) UpdateInstallHistory(ctx context.Context, id int, status, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if status == "success" || status == "failed" {
		_, err := s.db.ExecContext(ctx,
			`UPDATE install_history SET status=?, error_message=?, completed_at=? WHERE id=?`,
			status, errMsg, now, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE install_history SET status=?, error_message=? WHERE id=?`,
		status, errMsg, id)
	return err
}

func (s *sqliteStore) ListInstallHistory(ctx context.Context, limit int) ([]*InstallHistory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, skill_name, skill_version, source_url, source_type,
		        status, error_message, checksum, file_size, started_at, completed_at
		 FROM install_history ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*InstallHistory
	for rows.Next() {
		var h InstallHistory
		var sa string
		var ca *string
		if err := rows.Scan(&h.ID, &h.SkillName, &h.SkillVersion, &h.SourceURL, &h.SourceType,
			&h.Status, &h.ErrorMsg, &h.Checksum, &h.FileSize, &sa, &ca); err != nil {
			return nil, err
		}
		h.StartedAt, _ = time.Parse("2006-01-02 15:04:05", sa)
		if ca != nil && *ca != "" {
			t, _ := time.Parse("2006-01-02 15:04:05", *ca)
			h.CompletedAt = &t
		}
		out = append(out, &h)
	}
	return out, rows.Err()
}

// ── Stats ──────────────────────────────────────────────────────────────────

func (s *sqliteStore) Stats(ctx context.Context) (*Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var st Stats
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM installed_skills`).Scan(&st.SkillCount)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM market_sources`).Scan(&st.SourceCount)
	s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM installed_skills`).Scan(&st.TotalSize)
	return &st, nil
}

// =============================================================================
// Shared scan helpers
// =============================================================================

const selectSkillCols = `SELECT name, version, description, author, runtime, tags, tools,
	source_url, source_type, has_wasm, has_soi, size_bytes, checksum, status, install_time, updated_at`

func scanSkills(rows *sql.Rows) ([]*InstalledSkill, error) {
	var out []*InstalledSkill
	for rows.Next() {
		sk, err := scanOneSkillFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func scanOneSkill(row *sql.Row) (*InstalledSkill, error) {
	var sk InstalledSkill
	var tagsRaw, toolsRaw string
	var hasWasm, hasSoi int
	var it, ua string

	err := row.Scan(&sk.Name, &sk.Version, &sk.Description, &sk.Author, &sk.Runtime,
		&tagsRaw, &toolsRaw, &sk.SourceURL, &sk.SourceType,
		&hasWasm, &hasSoi, &sk.SizeBytes, &sk.Checksum, &sk.Status, &it, &ua)
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(tagsRaw), &sk.Tags)
	json.Unmarshal([]byte(toolsRaw), &sk.Tools)
	sk.HasWasm = hasWasm != 0
	sk.HasSoi = hasSoi != 0
	sk.InstallTime, _ = time.Parse("2006-01-02 15:04:05", it)
	sk.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", ua)
	return &sk, nil
}

func scanOneSkillFromRows(rows *sql.Rows) (*InstalledSkill, error) {
	var sk InstalledSkill
	var tagsRaw, toolsRaw string
	var hasWasm, hasSoi int
	var it, ua string

	err := rows.Scan(&sk.Name, &sk.Version, &sk.Description, &sk.Author, &sk.Runtime,
		&tagsRaw, &toolsRaw, &sk.SourceURL, &sk.SourceType,
		&hasWasm, &hasSoi, &sk.SizeBytes, &sk.Checksum, &sk.Status, &it, &ua)
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(tagsRaw), &sk.Tags)
	json.Unmarshal([]byte(toolsRaw), &sk.Tools)
	sk.HasWasm = hasWasm != 0
	sk.HasSoi = hasSoi != 0
	sk.InstallTime, _ = time.Parse("2006-01-02 15:04:05", it)
	sk.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", ua)
	return &sk, nil
}

// =============================================================================
// Tiny helpers
// =============================================================================

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func coalesceStrSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
