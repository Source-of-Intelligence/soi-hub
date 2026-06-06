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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v3"

	"github.com/Source-of-Intelligence/soi-hub/internal/skill"
	"github.com/Source-of-Intelligence/soi-hub/internal/skillmarket"
	skillpkg "github.com/Source-of-Intelligence/soi-hub/pkg/skill"
)

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
	httpServer := &http.Server{Addr: cfg.listenAddr(), Handler: loggingMiddleware(corsMiddleware(router)), ReadTimeout: 30 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second}
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
	return localSkill{Name: sk.Name, Version: sk.Version, Description: sk.Description, Author: sk.Author, Runtime: sk.Runtime, Tags: sk.Tags, Tools: sk.Tools, Path: sk.Name, HasWasm: sk.HasWasm, HasSoi: sk.HasSoi, Size: sk.SizeBytes, Source: "market"}
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
		s.store.UpsertSkill(ctx, &skillmarket.InstalledSkill{Name: sk.Name, Version: sk.Version, Description: sk.Description, Author: sk.Author, Runtime: sk.Runtime, Tags: sk.Tags, Tools: sk.Tools, HasWasm: sk.HasWasm, HasSoi: sk.HasSoi, SizeBytes: sk.Size, SourceType: "local", InstallTime: time.Now()})
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

func (s *Server) setupRoutes() {
	r := s.router
	r.HandleFunc("/health", s.handleHealth).Methods("GET")
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/market/search", s.handleMarketSearch).Methods("GET")
	api.HandleFunc("/market/sources", s.handleListSources).Methods("GET")
	api.HandleFunc("/market/sources", s.handleAddSource).Methods("POST")
	api.HandleFunc("/market/sources/{name}", s.handleRemoveSource).Methods("DELETE")
	api.HandleFunc("/skills", s.handleListSkills).Methods("GET")
	api.HandleFunc("/skills/search", s.handleSearchSkills).Methods("GET")
	api.HandleFunc("/skills/{name}", s.handleGetSkill).Methods("GET")
	api.HandleFunc("/skills/{name}/download", s.handleDownloadSkill).Methods("GET")
	api.HandleFunc("/skills/{name}/download/{file:.*}", s.handleDownloadSkillFile).Methods("GET")
	api.HandleFunc("/skills/import", s.handleImportSkill).Methods("POST")
	api.HandleFunc("/stats", s.handleStats).Methods("GET")
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "service": "skill-market"})
}

func (s *Server) handleMarketSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	results, err := s.market.Search(r.Context(), query)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if results == nil {
		results = []*skillpkg.SearchResult{}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "query": query, "count": len(results), "results": results})
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, _ := s.store.ListSources(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(srcs), "sources": srcs})
}

func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var src skillpkg.Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if src.Name == "" || src.URL == "" {
		writeError(w, 400, "name and url are required")
		return
	}
	if src.Type == "" {
		src.Type = "http"
	}
	if src.Priority == 0 {
		src.Priority = 5
	}
	src.Enabled = true
	s.store.AddSource(r.Context(), &skillmarket.MarketSource{Name: src.Name, URL: src.URL, Type: src.Type, Enabled: src.Enabled, Priority: src.Priority})
	s.market.AddSource(&src)
	writeJSON(w, 201, map[string]any{"ok": true, "data": src})
}

func (s *Server) handleRemoveSource(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if err := s.store.RemoveSource(r.Context(), name); err != nil {
		code := 500
		if strings.Contains(err.Error(), "not found") {
			code = 404
		}
		writeError(w, code, err.Error())
		return
	}
	s.market.RemoveSource(name)
	writeJSON(w, 200, map[string]any{"ok": true, "removed": name})
}

func (s *Server) handleListSkills(w http.ResponseWriter, _ *http.Request) {
	skills := s.getCachedSkills()
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(skills), "items": skills})
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if sk, err := s.store.GetSkill(r.Context(), name); err == nil && sk != nil {
		writeJSON(w, 200, map[string]any{"ok": true, "data": toLocalSkill(sk)})
		return
	}
	for _, ls := range s.getCachedSkills() {
		if ls.Name == name {
			writeJSON(w, 200, map[string]any{"ok": true, "data": ls})
			return
		}
	}
	writeError(w, 404, "skill not found: "+name)
}

func (s *Server) handleSearchSkills(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		cached := s.getCachedSkills()
		writeJSON(w, 200, map[string]any{"ok": true, "query": "", "count": len(cached), "results": cached})
		return
	}
	if dbSkills, _ := s.store.SearchSkills(r.Context(), query); len(dbSkills) > 0 {
		out := make([]localSkill, 0, len(dbSkills))
		for _, sk := range dbSkills {
			out = append(out, toLocalSkill(sk))
		}
		writeJSON(w, 200, map[string]any{"ok": true, "query": query, "count": len(out), "results": out})
		return
	}
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
	writeJSON(w, 200, map[string]any{"ok": true, "query": query, "count": len(out), "results": out})
}

func (s *Server) handleDownloadSkill(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	skillDir := filepath.Join(s.skillsDir, name)
	if !dirExists(skillDir) {
		writeError(w, 404, "skill not found: "+name)
		return
	}
	tmpDir, _ := os.MkdirTemp("", "soi-market-*")
	defer os.RemoveAll(tmpDir)
	zipPath := filepath.Join(tmpDir, name+".zip")
	if err := createZip(skillDir, name, zipPath); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	zipData, _ := os.ReadFile(zipPath)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipData)))
	w.Write(zipData)
}

func (s *Server) handleDownloadSkillFile(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	file := mux.Vars(r)["file"]
	absPath := filepath.Join(s.skillsDir, name, file)
	skillDir := filepath.Join(s.skillsDir, name)
	if !strings.HasPrefix(filepath.Clean(absPath), filepath.Clean(skillDir)) {
		writeError(w, 403, "path traversal denied")
		return
	}
	if !fileExists(absPath) {
		writeError(w, 404, "file not found: "+file)
		return
	}
	http.ServeFile(w, r, absPath)
}

func (s *Server) handleImportSkill(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(50 << 20)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "file field required")
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".zip" && ext != ".yaml" && ext != ".yml" && ext != ".md" {
		writeError(w, 400, "unsupported format: "+ext)
		return
	}
	overwrite := r.FormValue("overwrite") == "true"
	tmpFile, _ := os.CreateTemp("", "soi-market-import-*"+ext)
	defer os.Remove(tmpFile.Name())
	if _, err := io.Copy(tmpFile, file); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		writeError(w, 500, "write upload: "+err.Error())
		return
	}
	tmpFile.Close()
	var skillName string
	if ext == ".zip" {
		skillName, err = extractZip(tmpFile.Name(), s.skillsDir, overwrite)
	} else {
		skillName, err = installYAMLFile(tmpFile.Name(), s.skillsDir, overwrite)
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	skDir := filepath.Join(s.skillsDir, skillName)
	newSK := localSkill{Name: skillName, Path: skillName, Size: dirSize(skDir), Source: "market"}
	if yd, _ := os.ReadFile(filepath.Join(skDir, "skill.yaml")); yd != nil {
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
	s.store.UpsertSkill(r.Context(), &skillmarket.InstalledSkill{Name: newSK.Name, Version: newSK.Version, Description: newSK.Description, Author: newSK.Author, Runtime: newSK.Runtime, Tags: newSK.Tags, Tools: newSK.Tools, HasWasm: newSK.HasWasm, HasSoi: newSK.HasSoi, SizeBytes: newSK.Size, SourceType: "market", InstallTime: time.Now()})
	slog.Info("skill imported via market", "name", skillName)
	writeJSON(w, 200, map[string]any{"ok": true, "name": skillName})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	st, _ := s.store.Stats(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true, "stats": map[string]any{"skill_count": st.SkillCount, "source_count": st.SourceCount, "total_size": st.TotalSize, "market_sources": len(s.market.ListSources())}})
}

func scanLocalSkills(dir string) []localSkill {
	entries, _ := os.ReadDir(dir)
	var out []localSkill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(dir, e.Name())
		sk := localSkill{Name: e.Name(), Path: e.Name(), Source: "market"}
		if yd, _ := os.ReadFile(filepath.Join(d, "skill.yaml")); yd != nil {
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
		rel, _ := filepath.Rel(srcDir, path)
		zf, err := w.Create(filepath.ToSlash(filepath.Join(prefix, rel)))
		if err != nil {
			return err
		}
		data, _ := os.ReadFile(path)
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
	var name string
	for _, f := range r.File {
		if p := strings.SplitN(f.Name, "/", 2); p[0] != "" && p[0] != "." {
			name = p[0]
			break
		}
	}
	if name == "" {
		return "", fmt.Errorf("cannot determine skill name from zip")
	}
	target := filepath.Join(destDir, name)
	if !override && dirExists(target) {
		return "", fmt.Errorf("skill %q already exists (use overwrite=true)", name)
	}
	os.RemoveAll(target)
	os.MkdirAll(target, 0755)
	for _, f := range r.File {
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
		rc, _ := f.Open()
		if rc == nil {
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
	target := filepath.Join(destDir, name+".yaml")
	if !override && fileExists(target) {
		return "", fmt.Errorf("skill %q already exists (use overwrite=true)", name)
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

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
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
		slog.Log(context.Background(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", lrw.status, "duration_ms", dur.Milliseconds())
	})
}

type loggingRW struct {
	http.ResponseWriter
	status int
}

func (l *loggingRW) WriteHeader(code int) { l.status = code; l.ResponseWriter.WriteHeader(code) }

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"ok": "false", "error": msg})
}

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
