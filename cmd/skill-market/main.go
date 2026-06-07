// Package main provides a standalone Skill Market HTTP API service.
//
// This service is independent from the main SOI agent runtime. It focuses
// exclusively on skill discovery (search) and distribution (download/install).
//
// Build:
//
//	go build -o skill-market.exe ./cmd/skill-market
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v3"

	"github.com/Source-of-Intelligence/soi-hub/internal/pkg/auth"
	"github.com/Source-of-Intelligence/soi-hub/internal/pkg/validate"
	"github.com/Source-of-Intelligence/soi-hub/internal/skill"
	"github.com/Source-of-Intelligence/soi-hub/internal/skillmarket"
	skillpkg "github.com/Source-of-Intelligence/soi-hub/pkg/skill"
)

// Server is the HTTP server for the skill market service.
type Server struct {
	cfg       *config
	market    *skill.Market
	store     skillmarket.Store
	skillsDir string
	router    *mux.Router
	indexMu   sync.RWMutex
	index     []localSkill
}

func main() {
	var cfgFile, cliHost string
	var cliPort int
	var cliSkills, cliDB string
	flag.StringVar(&cfgFile, "config", "", "Path to config file (YAML)")
	flag.StringVar(&cliHost, "host", "", "HTTP listen host")
	flag.IntVar(&cliPort, "port", 0, "HTTP listen port")
	flag.StringVar(&cliSkills, "skills-dir", "", "Skills directory")
	flag.StringVar(&cliDB, "db", "", "Database DSN")
	flag.Parse()
	cfg, err := loadConfig(cfgFile)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	cfg.applyCLI(cliHost, cliPort, cliSkills, cliDB)
	cfg.applyEnv()
	if err := cfg.validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	absSkillsDir := cfg.absSkillsDir()
	if err := os.MkdirAll(absSkillsDir, 0755); err != nil {
		log.Fatalf("failed to create skills dir %q: %v", absSkillsDir, err)
	}
	store, err := skillmarket.Open(cfg.Storage.DB)
	if err != nil {
		log.Fatalf("failed to open database %q: %v", cfg.Storage.DB, err)
	}
	defer store.Close()
	slog.Info("database connected", "dsn", cfg.Storage.DB, "backend", detectBackend(cfg.Storage.DB))
	market := skill.NewMarket()
	ctx := context.Background()
	srcs, srcErr := store.ListSources(ctx)
	if srcErr != nil {
		log.Fatalf("failed to load sources from DB: %v", srcErr)
	}
	if len(srcs) > 0 {
		for _, def := range market.ListSources() {
			market.RemoveSource(def.Name)
		}
		for _, s := range srcs {
			market.AddSource(&skillpkg.Source{Name: s.Name, URL: s.URL, Type: s.Type, Enabled: s.Enabled, Priority: s.Priority})
		}
		slog.Info("loaded sources from database", "count", len(srcs))
	} else {
		for _, s := range cfg.Market.Sources {
			if vErr := validate.SourceURL(s.URL); vErr != nil {
				slog.Warn("skipping invalid source URL", "name", s.Name, "error", vErr)
				continue
			}
			store.AddSource(ctx, &skillmarket.MarketSource{Name: s.Name, URL: s.URL, Type: s.Type, Enabled: s.Enabled, Priority: s.Priority})
			market.AddSource(&skillpkg.Source{Name: s.Name, URL: s.URL, Type: s.Type, Enabled: s.Enabled, Priority: s.Priority})
		}
		if len(cfg.Market.Sources) > 0 {
			slog.Info("seeded market sources from config", "count", len(cfg.Market.Sources))
		}
	}
	router := mux.NewRouter()
	srv := &Server{cfg: cfg, market: market, store: store, skillsDir: absSkillsDir, router: router}
	srv.setupRoutes()
	srv.rebuildIndex()

	// Build middleware chain: logging -> auth -> CORS -> router
	var handler http.Handler = router
	handler = corsMiddleware(handler, cfg.Auth.CORSOrigins)
	handler = auth.Middleware(cfg.Auth.APIKeys)(handler)
	handler = loggingMiddleware(handler)

	httpServer := &http.Server{
		Addr:         cfg.listenAddr(),
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		slog.Info("shutting down skill-market ...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}()
	slog.Info("skill-market starting", "addr", cfg.listenAddr(), "skills_dir", absSkillsDir)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	slog.Info("skill-market stopped")
}

func detectBackend(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return "postgres"
	}
	return "sqlite"
}

// localSkill is the JSON representation of an installed skill.
type localSkill struct {
	Name, Version, Description, Author, Runtime, Path, Source string
	Tags, Tools                                               []string
	HasWasm, HasSoi                                           bool
	Size                                                      int64
	Rating                                                    float64
	Downloads                                                 int
}

type yamlMeta struct {
	Metadata struct {
		Name, Version, Description, Author string
		Tags                               []string `yaml:"tags"`
	} `yaml:"metadata"`
	Spec struct {
		Runtime  struct{ Type string } `yaml:"runtime"`
		Provides struct {
			Tools []struct{ Name string } `yaml:"tools"`
		} `yaml:"provides"`
	} `yaml:"spec"`
}

func toLocalSkill(sk *skillmarket.InstalledSkill) localSkill {
	return localSkill{
		Name: sk.Name, Version: sk.Version, Description: sk.Description,
		Author: sk.Author, Runtime: sk.Runtime, Tags: sk.Tags, Tools: sk.Tools,
		Path: sk.Name, HasWasm: sk.HasWasm, HasSoi: sk.HasSoi,
		Size: sk.SizeBytes, Source: sk.SourceType,
	}
}

func (s *Server) rebuildIndex() {
	ctx := context.Background()
	if dbSkills, err := s.store.ListSkills(ctx); err == nil && len(dbSkills) > 0 {
		idx := make([]localSkill, 0, len(dbSkills))
		for _, sk := range dbSkills {
			idx = append(idx, toLocalSkill(sk))
		}
		s.indexMu.Lock()
		s.index = idx
		s.indexMu.Unlock()
		slog.Info("built skill index from db", "count", len(idx))
		return
	}
	idx := scanLocalSkills(s.skillsDir)
	s.indexMu.Lock()
	s.index = idx
	s.indexMu.Unlock()
	for _, sk := range idx {
		s.store.UpsertSkill(ctx, &skillmarket.InstalledSkill{
			Name: sk.Name, Version: sk.Version, Description: sk.Description,
			Author: sk.Author, Runtime: sk.Runtime, Tags: sk.Tags, Tools: sk.Tools,
			HasWasm: sk.HasWasm, HasSoi: sk.HasSoi, SizeBytes: sk.Size,
			SourceType: "local", InstallTime: time.Now(),
		})
	}
	slog.Info("built skill index from filesystem (synced to db)", "count", len(idx))
}

func (s *Server) getCachedSkills() []localSkill {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	out := make([]localSkill, len(s.index))
	copy(out, s.index)
	return out
}

func (s *Server) appendCachedSkill(sk localSkill) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	for i, e := range s.index {
		if e.Name == sk.Name {
			s.index[i] = sk
			return
		}
	}
	s.index = append(s.index, sk)
}

func (s *Server) removeCachedSkill(name string) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	for i, e := range s.index {
		if e.Name == name {
			s.index = append(s.index[:i], s.index[i+1:]...)
			return
		}
	}
}

// =============================================================================
// Routes
// =============================================================================

func (s *Server) setupRoutes() {
	r := s.router
	r.HandleFunc("/health", s.handleHealth).Methods("GET")
	api := r.PathPrefix("/api/v1").Subrouter()

	// Market
	api.HandleFunc("/market/search", s.handleMarketSearch).Methods("GET")
	api.HandleFunc("/market/sources", s.handleListSources).Methods("GET")
	api.HandleFunc("/market/sources", s.handleAddSource).Methods("POST")
	api.HandleFunc("/market/sources/{name}", s.handleGetSource).Methods("GET")
	api.HandleFunc("/market/sources/{name}", s.handleUpdateSource).Methods("PUT")
	api.HandleFunc("/market/sources/{name}", s.handleRemoveSource).Methods("DELETE")

	// Skills
	api.HandleFunc("/skills", s.handleListSkills).Methods("GET")
	api.HandleFunc("/skills/search", s.handleSearchSkills).Methods("GET")
	api.HandleFunc("/skills/{name}", s.handleGetSkill).Methods("GET")
	api.HandleFunc("/skills/{name}", s.handleDeleteSkill).Methods("DELETE")
	api.HandleFunc("/skills/{name}/download", s.handleDownloadSkill).Methods("GET")
	api.HandleFunc("/skills/{name}/download/{file:.*}", s.handleDownloadSkillFile).Methods("GET")
	api.HandleFunc("/skills/import", s.handleImportSkill).Methods("POST")

	// History
	api.HandleFunc("/history", s.handleListHistory).Methods("GET")
	api.HandleFunc("/skills/{name}/history", s.handleSkillHistory).Methods("GET")

	// Stats
	api.HandleFunc("/stats", s.handleStats).Methods("GET")
}

// =============================================================================
// Handlers
// =============================================================================

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "skill-market"})
}

// --- Market Search ---

func (s *Server) handleMarketSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if err := validate.SearchQuery(query); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	results, err := s.market.Search(r.Context(), query)
	if err != nil {
		slog.Error("market search failed", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error(), "INTERNAL_ERROR")
		return
	}
	if results == nil {
		results = []*skillpkg.SearchResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "query": query, "count": len(results), "results": results})
}

// --- Sources ---

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.store.ListSources(r.Context())
	if err != nil {
		slog.Error("failed to list sources", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list sources", "INTERNAL_ERROR")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(srcs), "sources": srcs})
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := validate.SourceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	srcs, err := s.store.ListSources(r.Context())
	if err != nil {
		slog.Error("failed to list sources", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list sources", "INTERNAL_ERROR")
		return
	}
	for _, src := range srcs {
		if src.Name == name {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": src})
			return
		}
	}
	writeError(w, http.StatusNotFound, "source not found: "+name, "NOT_FOUND")
}

func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var src skillpkg.Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "BAD_REQUEST")
		return
	}
	if err := validate.SourceName(src.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if err := validate.SourceURL(src.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if src.Type == "" {
		src.Type = "http"
	}
	if src.Priority == 0 {
		src.Priority = 5
	}
	if !src.Enabled {
		src.Enabled = true
	}
	if err := s.store.AddSource(r.Context(), &skillmarket.MarketSource{
		Name: src.Name, URL: src.URL, Type: src.Type,
		Enabled: src.Enabled, Priority: src.Priority,
	}); err != nil {
		slog.Error("failed to add source", "name", src.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to add source", "INTERNAL_ERROR")
		return
	}
	s.market.AddSource(&src)
	slog.Info("source added", "name", src.Name, "url", src.URL)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "data": src})
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := validate.SourceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	var req struct {
		Enabled  *bool `json:"enabled"`
		Priority *int  `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "BAD_REQUEST")
		return
	}

	// Get existing source
	srcs, err := s.store.ListSources(r.Context())
	if err != nil {
		slog.Error("failed to list sources", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list sources", "INTERNAL_ERROR")
		return
	}
	var existing *skillmarket.MarketSource
	for _, src := range srcs {
		if src.Name == name {
			existing = src
			break
		}
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "source not found: "+name, "NOT_FOUND")
		return
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		existing.Priority = *req.Priority
	}
	if err := s.store.AddSource(r.Context(), existing); err != nil {
		slog.Error("failed to update source", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update source", "INTERNAL_ERROR")
		return
	}
	// Update in-memory market
	s.market.RemoveSource(name)
	s.market.AddSource(&skillpkg.Source{
		Name: existing.Name, URL: existing.URL, Type: existing.Type,
		Enabled: existing.Enabled, Priority: existing.Priority,
	})
	slog.Info("source updated", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": existing})
}

func (s *Server) handleRemoveSource(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := validate.SourceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if err := s.store.RemoveSource(r.Context(), name); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error(), "NOT_FOUND")
			return
		}
		slog.Error("failed to remove source", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to remove source", "INTERNAL_ERROR")
		return
	}
	s.market.RemoveSource(name)
	slog.Info("source removed", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": name})
}

// --- Skills ---

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	skills := s.getCachedSkills()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(skills), "items": skills})
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := validate.SkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if sk, err := s.store.GetSkill(r.Context(), name); err == nil && sk != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": toLocalSkill(sk)})
		return
	}
	for _, ls := range s.getCachedSkills() {
		if ls.Name == name {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": ls})
			return
		}
	}
	writeError(w, http.StatusNotFound, "skill not found: "+name, "NOT_FOUND")
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := validate.SkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	// Check if skill exists
	if _, err := s.store.GetSkill(r.Context(), name); err != nil {
		writeError(w, http.StatusNotFound, "skill not found: "+name, "NOT_FOUND")
		return
	}
	// Remove from DB
	if err := s.store.DeleteSkill(r.Context(), name); err != nil {
		slog.Error("failed to delete skill", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete skill", "INTERNAL_ERROR")
		return
	}
	// Remove from filesystem
	skillDir := filepath.Join(s.skillsDir, name)
	if dirExists(skillDir) {
		if err := os.RemoveAll(skillDir); err != nil {
			slog.Error("failed to remove skill directory", "dir", skillDir, "error", err)
		}
	}
	// Remove from cache
	s.removeCachedSkill(name)
	slog.Info("skill deleted", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": name})
}

func (s *Server) handleSearchSkills(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if err := validate.SearchQuery(query); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if query == "" {
		cached := s.getCachedSkills()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "query": "", "count": len(cached), "results": cached})
		return
	}
	dbSkills, err := s.store.SearchSkills(r.Context(), query)
	if err != nil {
		slog.Error("database search failed", "query", query, "error", err)
		// Fall back to memory search
	} else if len(dbSkills) > 0 {
		out := make([]localSkill, 0, len(dbSkills))
		for _, sk := range dbSkills {
			out = append(out, toLocalSkill(sk))
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "query": query, "count": len(out), "results": out})
		return
	}
	// Fallback: memory search
	cached := s.getCachedSkills()
	pattern := strings.ToLower(query)
	var out []localSkill
	for _, sk := range cached {
		if strings.Contains(strings.ToLower(sk.Name), pattern) || strings.Contains(strings.ToLower(sk.Description), pattern) {
			out = append(out, sk)
		} else {
			for _, t := range sk.Tags {
				if strings.Contains(strings.ToLower(t), pattern) {
					out = append(out, sk)
					break
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "query": query, "count": len(out), "results": out})
}

func (s *Server) handleDownloadSkill(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := validate.SkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	skillDir := filepath.Join(s.skillsDir, name)
	if !dirExists(skillDir) {
		writeError(w, http.StatusNotFound, "skill not found: "+name, "NOT_FOUND")
		return
	}

	// Stream ZIP directly to response (no temp file, no memory buffer)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))
	zw := zip.NewWriter(w)
	defer zw.Close()

	if err := filepath.Walk(skillDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return err
		}
		zf, err := zw.Create(filepath.ToSlash(filepath.Join(name, rel)))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(zf, f)
		return err
	}); err != nil {
		slog.Error("failed to create zip", "name", name, "error", err)
		// Note: headers already sent, cannot change status code
		return
	}
}

func (s *Server) handleDownloadSkillFile(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	file := mux.Vars(r)["file"]
	if err := validate.SkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	absPath := filepath.Join(s.skillsDir, name, file)
	skillDir := filepath.Join(s.skillsDir, name)
	if !strings.HasPrefix(filepath.Clean(absPath), filepath.Clean(skillDir)) {
		writeError(w, http.StatusForbidden, "path traversal denied", "FORBIDDEN")
		return
	}
	if !fileExists(absPath) {
		writeError(w, http.StatusNotFound, "file not found: "+file, "NOT_FOUND")
		return
	}
	http.ServeFile(w, r, absPath)
}

func (s *Server) handleImportSkill(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error(), "BAD_REQUEST")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file field required", "BAD_REQUEST")
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".zip" && ext != ".yaml" && ext != ".yml" && ext != ".md" {
		writeError(w, http.StatusBadRequest, "unsupported format: "+ext, "BAD_REQUEST")
		return
	}
	overwrite := r.FormValue("overwrite") == "true"

	// Create temp file
	tmpFile, err := os.CreateTemp("", "soi-market-import-*"+ext)
	if err != nil {
		slog.Error("failed to create temp file", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create temp file", "INTERNAL_ERROR")
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, file); err != nil {
		slog.Error("failed to write upload", "error", err)
		writeError(w, http.StatusInternalServerError, "write upload: "+err.Error(), "INTERNAL_ERROR")
		return
	}
	tmpFile.Close()

	// Record install history start
	history := &skillmarket.InstallHistory{
		SkillName:  filepath.Base(header.Filename),
		SourceURL:  "upload",
		SourceType: "url",
		Status:     "downloading",
	}
	if err := s.store.AddInstallHistory(r.Context(), history); err != nil {
		slog.Error("failed to add install history", "error", err)
	}

	var skillName string
	if ext == ".zip" {
		skillName, err = extractZip(tmpFile.Name(), s.skillsDir, overwrite)
	} else {
		skillName, err = installYAMLFile(tmpFile.Name(), s.skillsDir, overwrite)
	}
	if err != nil {
		slog.Error("skill import failed", "error", err)
		if history.ID > 0 {
			s.store.UpdateInstallHistory(r.Context(), history.ID, "failed", err.Error())
		}
		writeError(w, http.StatusInternalServerError, err.Error(), "INTERNAL_ERROR")
		return
	}

	// Validate skill name
	if vErr := validate.SkillName(skillName); vErr != nil {
		if history.ID > 0 {
			s.store.UpdateInstallHistory(r.Context(), history.ID, "failed", vErr.Error())
		}
		writeError(w, http.StatusBadRequest, vErr.Error(), "BAD_REQUEST")
		return
	}

	skDir := filepath.Join(s.skillsDir, skillName)
	newSK := localSkill{Name: skillName, Path: skillName, Size: dirSize(skDir), Source: "market"}
	if yd, err := os.ReadFile(filepath.Join(skDir, "skill.yaml")); err == nil && yd != nil {
		populateSkillFromYAML(yd, &newSK)
	}
	if _, err := os.Stat(filepath.Join(skDir, "wasm", "plugin.wasm")); err == nil {
		newSK.HasWasm = true
	}
	if _, err := os.Stat(filepath.Join(skDir, "wasm", "plugin.soi")); err == nil {
		newSK.HasSoi = true
	}
	newSK.Size = dirSize(skDir)
	s.appendCachedSkill(newSK)
	if err := s.store.UpsertSkill(r.Context(), &skillmarket.InstalledSkill{
		Name: newSK.Name, Version: newSK.Version, Description: newSK.Description,
		Author: newSK.Author, Runtime: newSK.Runtime, Tags: newSK.Tags, Tools: newSK.Tools,
		HasWasm: newSK.HasWasm, HasSoi: newSK.HasSoi, SizeBytes: newSK.Size,
		SourceType: "market", InstallTime: time.Now(),
	}); err != nil {
		slog.Error("failed to upsert skill", "name", skillName, "error", err)
	}
	if history.ID > 0 {
		s.store.UpdateInstallHistory(r.Context(), history.ID, "success", "")
	}
	slog.Info("skill imported via market", "name", skillName)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": skillName})
}

// --- History ---

func (s *Server) handleListHistory(w http.ResponseWriter, r *http.Request) {
	history, err := s.store.ListInstallHistory(r.Context(), 0)
	if err != nil {
		slog.Error("failed to list history", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list history", "INTERNAL_ERROR")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(history), "items": history})
}

func (s *Server) handleSkillHistory(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := validate.SkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	allHistory, err := s.store.ListInstallHistory(r.Context(), 0)
	if err != nil {
		slog.Error("failed to list history", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list history", "INTERNAL_ERROR")
		return
	}
	var filtered []*skillmarket.InstallHistory
	for _, h := range allHistory {
		if h.SkillName == name {
			filtered = append(filtered, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(filtered), "items": filtered})
}

// --- Stats ---

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	st, err := s.store.Stats(context.Background())
	if err != nil {
		slog.Error("failed to get stats", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get stats", "INTERNAL_ERROR")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"stats": map[string]any{
			"skill_count":    st.SkillCount,
			"source_count":   st.SourceCount,
			"total_size":     st.TotalSize,
			"market_sources": len(s.market.ListSources()),
		},
	})
}

// =============================================================================
// Helpers
// =============================================================================

func scanLocalSkills(dir string) []localSkill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Error("failed to read skills dir", "dir", dir, "error", err)
		return nil
	}
	var out []localSkill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(dir, e.Name())
		sk := localSkill{Name: e.Name(), Path: e.Name(), Source: "market"}
		if yd, err := os.ReadFile(filepath.Join(d, "skill.yaml")); err == nil && yd != nil {
			populateSkillFromYAML(yd, &sk)
		}
		if _, err := os.Stat(filepath.Join(d, "wasm", "plugin.wasm")); err == nil {
			sk.HasWasm = true
		}
		if _, err := os.Stat(filepath.Join(d, "wasm", "plugin.soi")); err == nil {
			sk.HasSoi = true
		}
		sk.Size = dirSize(d)
		out = append(out, sk)
	}
	return out
}

func populateSkillFromYAML(data []byte, sk *localSkill) {
	var ym yamlMeta
	if err := yaml.Unmarshal(data, &ym); err != nil {
		return
	}
	sk.Version = ym.Metadata.Version
	sk.Description = ym.Metadata.Description
	sk.Author = ym.Metadata.Author
	sk.Runtime = ym.Spec.Runtime.Type
	sk.Tags = ym.Metadata.Tags
	for _, t := range ym.Spec.Provides.Tools {
		sk.Tools = append(sk.Tools, t.Name)
	}
}

func createZip(srcDir, prefix, dest string) error {
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create zip: %w", err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	defer w.Close()
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		zf, err := w.Create(filepath.ToSlash(filepath.Join(prefix, rel)))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = zf.Write(data)
		return err
	})
}

func extractZip(zipPath, destDir string, override bool) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	// ZIP bomb protection
	limits := validate.DefaultZipLimits
	var totalSize int64
	var fileCount int

	var name string
	var incomingVersion string
	// 先查找 skill.yaml 文件以获取版本号
	for _, f := range r.File {
		if strings.HasSuffix(f.Name, "/skill.yaml") || f.Name == "skill.yaml" {
			// 获取技能名称
			if name == "" {
				if p := strings.SplitN(f.Name, "/", 2); len(p) > 1 && p[0] != "" && p[0] != "." {
					name = p[0]
				} else if p := strings.SplitN(f.Name, "/", 2); len(p) == 1 {
					// 如果没有目录结构，先从文件名推断，但我们还是先读取内容
					name = ""
				}
			}
			// 读取 skill.yaml 内容
			rc, err := f.Open()
			if err == nil {
				var yamlData []byte
				yamlData, err = io.ReadAll(rc)
				rc.Close()
				if err == nil {
					var ym yamlMeta
					if yaml.Unmarshal(yamlData, &ym) == nil {
						if name == "" {
							name = ym.Metadata.Name
						}
						incomingVersion = ym.Metadata.Version
					}
				}
			}
		}
		if name == "" {
			if p := strings.SplitN(f.Name, "/", 2); p[0] != "" && p[0] != "." {
				name = p[0]
			}
		}
	}
	if name == "" {
		return "", fmt.Errorf("cannot determine skill name from zip")
	}

	target := filepath.Join(destDir, name)
	if dirExists(target) {
		// 检查是否需要覆盖
		if override {
			// 强制覆盖
			os.RemoveAll(target)
		} else {
			// 获取现有版本
			var existingVersion string
			existingSkillYaml := filepath.Join(target, "skill.yaml")
			if fileExists(existingSkillYaml) {
				if yamlData, err := os.ReadFile(existingSkillYaml); err == nil {
					var ym yamlMeta
					if yaml.Unmarshal(yamlData, &ym) == nil {
						existingVersion = ym.Metadata.Version
					}
				}
			}

			// 比较版本
			compResult := compareVersions(incomingVersion, existingVersion)
			if compResult <= 0 {
				if compResult == 0 {
					return "", fmt.Errorf("skill %q with version %q already exists and is not newer", name, incomingVersion)
				}
				return "", fmt.Errorf("skill %q with version %q is not newer than existing version %q", name, incomingVersion, existingVersion)
			}
			// 版本更高，可以覆盖
			os.RemoveAll(target)
		}
	}
	os.MkdirAll(target, 0755)

	for _, f := range r.File {
		fileCount++
		if fileCount > limits.MaxFileCount {
			os.RemoveAll(target)
			return "", fmt.Errorf("zip contains too many files (max %d)", limits.MaxFileCount)
		}
		if f.UncompressedSize64 > uint64(limits.MaxFileSize) {
			os.RemoveAll(target)
			return "", fmt.Errorf("zip file %q exceeds max size %d bytes", f.Name, limits.MaxFileSize)
		}
		totalSize += int64(f.UncompressedSize64)
		if totalSize > limits.MaxTotalSize {
			os.RemoveAll(target)
			return "", fmt.Errorf("zip total uncompressed size exceeds %d bytes", limits.MaxTotalSize)
		}
		if len(f.Name) > limits.MaxPathLength {
			os.RemoveAll(target)
			return "", fmt.Errorf("zip path too long: %q", f.Name)
		}

		parts := strings.SplitN(f.Name, "/", 2)
		rel := ""
		if len(parts) > 1 {
			rel = parts[1]
		}
		if rel == "" {
			continue
		}
		dest := filepath.Join(target, rel)
		if !strings.HasPrefix(filepath.Clean(dest), filepath.Clean(target)+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(dest, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(dest), 0755)
		rc, err := f.Open()
		if err != nil {
			continue
		}
		out, err := os.Create(dest)
		if err != nil {
			rc.Close()
			continue
		}
		io.Copy(out, rc)
		out.Close()
		rc.Close()
	}
	return name, nil
}

func installYAMLFile(src, destDir string, override bool) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	name := extractYAMLName(data)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	}
	// 获取新版本号
	var incomingVersion string
	var ym yamlMeta
	if yaml.Unmarshal(data, &ym) == nil {
		incomingVersion = ym.Metadata.Version
	}

	target := filepath.Join(destDir, name+".yaml")
	if fileExists(target) {
		// 检查是否需要覆盖
		if override {
			// 强制覆盖
		} else {
			// 获取现有版本
			var existingVersion string
			if existingData, err := os.ReadFile(target); err == nil {
				var existingYaml yamlMeta
				if yaml.Unmarshal(existingData, &existingYaml) == nil {
					existingVersion = existingYaml.Metadata.Version
				}
			}

			// 比较版本
			compResult := compareVersions(incomingVersion, existingVersion)
			if compResult <= 0 {
				if compResult == 0 {
					return "", fmt.Errorf("skill %q with version %q already exists and is not newer", name, incomingVersion)
				}
				return "", fmt.Errorf("skill %q with version %q is not newer than existing version %q", name, incomingVersion, existingVersion)
			}
		}
	}
	if err := os.WriteFile(target, data, 0644); err != nil {
		return "", err
	}
	return name, nil
}

func extractYAMLName(data []byte) string {
	lines := strings.Split(string(data), "\n")
	inMeta := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "metadata:" {
			inMeta = true
			continue
		}
		if inMeta && strings.HasPrefix(t, "name:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "name:")), `"'`)
		}
		if inMeta && len(t) > 0 && !strings.HasPrefix(t, "  ") {
			return ""
		}
	}
	return ""
}

// =============================================================================
// Middleware
// =============================================================================

func corsMiddleware(next http.Handler, allowedOrigins []string) http.Handler {
	allowAll := false
	originMap := make(map[string]struct{})
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
			break
		}
		originMap[o] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowAll {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin != "" {
			if _, ok := originMap[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingRW{ResponseWriter: w, status: 200}
		next.ServeHTTP(lrw, r)
		dur := time.Since(start)
		lvl := slog.LevelInfo
		if lrw.status >= 500 {
			lvl = slog.LevelError
		} else if lrw.status >= 400 {
			lvl = slog.LevelWarn
		}
		slog.Log(context.Background(), lvl, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.status,
			"duration_ms", dur.Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

type loggingRW struct {
	http.ResponseWriter
	status int
}

func (l *loggingRW) WriteHeader(code int) { l.status = code; l.ResponseWriter.WriteHeader(code) }

// =============================================================================
// Response helpers
// =============================================================================

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string, code string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg, "code": code})
}

// isNotFound checks if an error indicates a "not found" condition.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}

// =============================================================================
// File helpers
// =============================================================================

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirSize(p string) int64 {
	var s int64
	filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			s += fi.Size()
		}
		return nil
	})
	return s
}

// compareVersions 比较两个版本号
// 返回 1 表示 v1 > v2
// 返回 -1 表示 v1 < v2
// 返回 0 表示 v1 == v2
func compareVersions(v1, v2 string) int {
	// 处理空版本号
	if v1 == "" && v2 == "" {
		return 0
	}
	if v1 == "" {
		return -1
	}
	if v2 == "" {
		return 1
	}

	// 分割版本号
	parts1 := strings.Split(strings.TrimSpace(v1), ".")
	parts2 := strings.Split(strings.TrimSpace(v2), ".")

	// 比较每一部分
	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var p1, p2 int
		if i < len(parts1) {
			p1, _ = strconv.Atoi(parts1[i])
		}
		if i < len(parts2) {
			p2, _ = strconv.Atoi(parts2[i])
		}

		if p1 > p2 {
			return 1
		} else if p1 < p2 {
			return -1
		}
	}

	return 0
}
