// Package skillmarket provides persistence for the skill market service.
//
// Supports two backends:
//   - sqlite (default, zero-config, via modernc.org/sqlite)
//   - postgres (when DSN starts with postgres:// or postgresql://)
//
// The Store interface is the single entry point for all persistence needs:
// market sources, installed skill metadata, and install history.
package skillmarket

import (
	"context"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// =============================================================================
// Types
// =============================================================================

// MarketSource represents a skill registry source persisted to DB.
type MarketSource struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Type      string    `json:"type"`
	Enabled   bool      `json:"enabled"`
	Priority  int       `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// InstalledSkill is the DB-backed representation of a skill installed on disk.
type InstalledSkill struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	Author      string    `json:"author,omitempty"`
	Runtime     string    `json:"runtime"`
	Tags        []string  `json:"tags,omitempty"`
	Tools       []string  `json:"tools,omitempty"`
	SourceURL   string    `json:"source_url,omitempty"`
	SourceType  string    `json:"source_type"` // local, market, url, git
	HasWasm     bool      `json:"has_wasm"`
	HasSoi      bool      `json:"has_soi"`
	SizeBytes   int64     `json:"size_bytes"`
	Checksum    string    `json:"checksum,omitempty"`
	Status      string    `json:"status"` // active, inactive, error
	InstallTime time.Time `json:"install_time"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// InstallHistory records a skill download/install event.
type InstallHistory struct {
	ID           int        `json:"id"`
	SkillName    string     `json:"skill_name"`
	SkillVersion string     `json:"skill_version,omitempty"`
	SourceURL    string     `json:"source_url"`
	SourceType   string     `json:"source_type"` // url, market
	Status       string     `json:"status"`      // pending, downloading, success, failed
	ErrorMsg     string     `json:"error_message,omitempty"`
	Checksum     string     `json:"checksum,omitempty"`
	FileSize     int64      `json:"file_size"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// Stats is a simple aggregate returned by Store.Stats.
type Stats struct {
	SkillCount  int   `json:"skill_count"`
	SourceCount int   `json:"source_count"`
	TotalSize   int64 `json:"total_size_bytes"`
}

// =============================================================================
// Store interface
// =============================================================================

// Store is the unified persistence interface for the skill market service.
type Store interface {
	Close() error

	ListSources(ctx context.Context) ([]*MarketSource, error)
	AddSource(ctx context.Context, src *MarketSource) error
	RemoveSource(ctx context.Context, name string) error

	ListSkills(ctx context.Context) ([]*InstalledSkill, error)
	GetSkill(ctx context.Context, name string) (*InstalledSkill, error)
	UpsertSkill(ctx context.Context, sk *InstalledSkill) error
	DeleteSkill(ctx context.Context, name string) error
	SearchSkills(ctx context.Context, query string) ([]*InstalledSkill, error)

	AddInstallHistory(ctx context.Context, h *InstallHistory) error
	UpdateInstallHistory(ctx context.Context, id int, status string, errMsg string) error
	ListInstallHistory(ctx context.Context, limit int) ([]*InstallHistory, error)

	Stats(ctx context.Context) (*Stats, error)
}

// =============================================================================
// Factory
// =============================================================================

// Open opens a Store based on the DSN.
//
//   - SQLite:  "skill-market.db"  or  "file:data/sm.db?_journal=WAL"
//   - Postgres: "postgres://user:pass@host:5432/dbname?sslmode=disable"
func Open(dsn string) (Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("dsn is empty")
	}
	if isPostgresDSN(dsn) {
		return newPostgresStore(dsn)
	}
	return newSQLiteStore(dsn)
}

func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") ||
		strings.HasPrefix(dsn, "postgresql://")
}
