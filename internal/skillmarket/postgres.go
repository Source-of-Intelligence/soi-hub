package skillmarket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// =============================================================================
// PostgreSQL schema
// =============================================================================

const pgSchema = `
CREATE TABLE IF NOT EXISTS market_sources (
    id         SERIAL PRIMARY KEY,
    name       VARCHAR(255) UNIQUE NOT NULL,
    url        TEXT    NOT NULL,
    type       VARCHAR(50)  DEFAULT 'http',
    enabled    BOOLEAN DEFAULT TRUE,
    priority   INTEGER DEFAULT 5,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS installed_skills (
    name        VARCHAR(255) PRIMARY KEY,
    version     VARCHAR(50)  DEFAULT '1.0.0',
    description TEXT    DEFAULT '',
    author      VARCHAR(255) DEFAULT '',
    runtime     VARCHAR(50)  DEFAULT 'builtin',
    tags        JSONB   DEFAULT '[]'::jsonb,
    tools       JSONB   DEFAULT '[]'::jsonb,
    source_url  TEXT    DEFAULT '',
    source_type VARCHAR(50)  DEFAULT 'local',
    has_wasm    BOOLEAN DEFAULT FALSE,
    has_soi     BOOLEAN DEFAULT FALSE,
    size_bytes  BIGINT  DEFAULT 0,
    checksum    VARCHAR(128) DEFAULT '',
    status      VARCHAR(50)  DEFAULT 'active',
    install_time TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS install_history (
    id            SERIAL PRIMARY KEY,
    skill_name    VARCHAR(255) NOT NULL,
    skill_version VARCHAR(50)  DEFAULT '',
    source_url    TEXT    NOT NULL,
    source_type   VARCHAR(50)  DEFAULT 'url',
    status        VARCHAR(50)  DEFAULT 'pending',
    error_message TEXT    DEFAULT '',
    checksum      VARCHAR(128) DEFAULT '',
    file_size     BIGINT  DEFAULT 0,
    started_at    TIMESTAMPTZ DEFAULT NOW(),
    completed_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_ih_skill  ON install_history(skill_name);
CREATE INDEX IF NOT EXISTS idx_ih_status ON install_history(status);
`

// =============================================================================
// pgStore
// =============================================================================

type pgStore struct {
	db *sql.DB
}

func newPostgresStore(dsn string) (*pgStore, error) {
	// Normalize: postgresql:// → postgres://
	if strings.HasPrefix(dsn, "postgresql://") {
		dsn = "postgres://" + dsn[len("postgresql://"):]
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	store := &pgStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	return store, nil
}

func (p *pgStore) migrate() error {
	_, err := p.db.Exec(pgSchema)
	return err
}

func (p *pgStore) Close() error { return p.db.Close() }

// ── Sources ────────────────────────────────────────────────────────────────

func (p *pgStore) ListSources(ctx context.Context) ([]*MarketSource, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, name, url, type, enabled, priority, created_at, updated_at
		 FROM market_sources ORDER BY priority DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*MarketSource
	for rows.Next() {
		var m MarketSource
		if err := rows.Scan(&m.ID, &m.Name, &m.URL, &m.Type, &m.Enabled, &m.Priority, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (p *pgStore) AddSource(ctx context.Context, src *MarketSource) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO market_sources (name, url, type, enabled, priority, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (name) DO UPDATE SET
			url=$2, type=$3, enabled=$4, priority=$5, updated_at=NOW()`,
		src.Name, src.URL, src.Type, src.Enabled, src.Priority)
	return err
}

func (p *pgStore) RemoveSource(ctx context.Context, name string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM market_sources WHERE name = $1`, name)
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

const pgSelectSkillCols = `name, version, description, author, runtime,
	tags::text, tools::text, source_url, source_type,
	has_wasm, has_soi, size_bytes, checksum, status, install_time, updated_at`

func (p *pgStore) ListSkills(ctx context.Context) ([]*InstalledSkill, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+pgSelectSkillCols+` FROM installed_skills ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSkillsPg(rows)
}

func (p *pgStore) GetSkill(ctx context.Context, name string) (*InstalledSkill, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT `+pgSelectSkillCols+` FROM installed_skills WHERE name = $1`, name)
	return scanOneSkillPg(row)
}

func (p *pgStore) UpsertSkill(ctx context.Context, sk *InstalledSkill) error {
	if sk.Status == "" {
		sk.Status = "active"
	}
	if sk.Runtime == "" {
		sk.Runtime = "builtin"
	}
	if sk.SourceType == "" {
		sk.SourceType = "local"
	}

	tagsJSON, _ := json.Marshal(coalesceStrSlice(sk.Tags))
	toolsJSON, _ := json.Marshal(coalesceStrSlice(sk.Tools))

	_, err := p.db.ExecContext(ctx,
		`INSERT INTO installed_skills
			(name, version, description, author, runtime, tags, tools,
			 source_url, source_type, has_wasm, has_soi, size_bytes, checksum, status, install_time, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb,
		         $8, $9, $10, $11, $12, $13, $14, NOW(), NOW())
		 ON CONFLICT (name) DO UPDATE SET
			version=$2, description=$3, author=$4, runtime=$5,
			tags=$6::jsonb, tools=$7::jsonb,
			source_url=$8, source_type=$9,
			has_wasm=$10, has_soi=$11, size_bytes=$12, checksum=$13, status=$14,
			updated_at=NOW()`,
		sk.Name, sk.Version, sk.Description, sk.Author, sk.Runtime,
		string(tagsJSON), string(toolsJSON),
		sk.SourceURL, sk.SourceType, sk.HasWasm, sk.HasSoi,
		sk.SizeBytes, sk.Checksum, sk.Status)
	return err
}

func (p *pgStore) DeleteSkill(ctx context.Context, name string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM installed_skills WHERE name = $1`, name)
	return err
}

func (p *pgStore) SearchSkills(ctx context.Context, query string) ([]*InstalledSkill, error) {
	pattern := "%" + query + "%"
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+pgSelectSkillCols+` FROM installed_skills
		 WHERE name ILIKE $1 OR description ILIKE $1 OR author ILIKE $1
		 ORDER BY name ASC`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSkillsPg(rows)
}

// ── Install history ────────────────────────────────────────────────────────

func (p *pgStore) AddInstallHistory(ctx context.Context, h *InstallHistory) error {
	var id int
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO install_history (skill_name, skill_version, source_url, source_type, checksum, started_at)
		 VALUES ($1, $2, $3, $4, $5, NOW()) RETURNING id`,
		h.SkillName, h.SkillVersion, h.SourceURL, h.SourceType, h.Checksum,
	).Scan(&id)
	if err != nil {
		return err
	}
	h.ID = id
	h.StartedAt = time.Now().UTC()
	return nil
}

func (p *pgStore) UpdateInstallHistory(ctx context.Context, id int, status, errMsg string) error {
	if status == "success" || status == "failed" {
		_, err := p.db.ExecContext(ctx,
			`UPDATE install_history SET status=$1, error_message=$2, completed_at=NOW() WHERE id=$3`,
			status, errMsg, id)
		return err
	}
	_, err := p.db.ExecContext(ctx,
		`UPDATE install_history SET status=$1, error_message=$2 WHERE id=$3`,
		status, errMsg, id)
	return err
}

func (p *pgStore) ListInstallHistory(ctx context.Context, limit int) ([]*InstallHistory, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, skill_name, skill_version, source_url, source_type,
		        status, error_message, checksum, file_size, started_at, completed_at
		 FROM install_history ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*InstallHistory
	for rows.Next() {
		var h InstallHistory
		if err := rows.Scan(&h.ID, &h.SkillName, &h.SkillVersion, &h.SourceURL, &h.SourceType,
			&h.Status, &h.ErrorMsg, &h.Checksum, &h.FileSize, &h.StartedAt, &h.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, &h)
	}
	return out, rows.Err()
}

// ── Stats ──────────────────────────────────────────────────────────────────

func (p *pgStore) Stats(ctx context.Context) (*Stats, error) {
	var st Stats
	p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM installed_skills`).Scan(&st.SkillCount)
	p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM market_sources`).Scan(&st.SourceCount)
	p.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM installed_skills`).Scan(&st.TotalSize)
	return &st, nil
}

// =============================================================================
// PostgreSQL scan helpers
// =============================================================================

func scanSkillsPg(rows *sql.Rows) ([]*InstalledSkill, error) {
	var out []*InstalledSkill
	for rows.Next() {
		sk, err := scanOneSkillFromRowsPg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func scanOneSkillPg(row *sql.Row) (*InstalledSkill, error) {
	var sk InstalledSkill
	var tagsRaw, toolsRaw string

	err := row.Scan(&sk.Name, &sk.Version, &sk.Description, &sk.Author, &sk.Runtime,
		&tagsRaw, &toolsRaw, &sk.SourceURL, &sk.SourceType,
		&sk.HasWasm, &sk.HasSoi, &sk.SizeBytes, &sk.Checksum, &sk.Status,
		&sk.InstallTime, &sk.UpdatedAt)
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(tagsRaw), &sk.Tags)
	json.Unmarshal([]byte(toolsRaw), &sk.Tools)
	return &sk, nil
}

func scanOneSkillFromRowsPg(rows *sql.Rows) (*InstalledSkill, error) {
	var sk InstalledSkill
	var tagsRaw, toolsRaw string

	err := rows.Scan(&sk.Name, &sk.Version, &sk.Description, &sk.Author, &sk.Runtime,
		&tagsRaw, &toolsRaw, &sk.SourceURL, &sk.SourceType,
		&sk.HasWasm, &sk.HasSoi, &sk.SizeBytes, &sk.Checksum, &sk.Status,
		&sk.InstallTime, &sk.UpdatedAt)
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(tagsRaw), &sk.Tags)
	json.Unmarshal([]byte(toolsRaw), &sk.Tools)
	return &sk, nil
}
