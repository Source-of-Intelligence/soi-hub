// Package skillhub provides core functions to search and install skills.
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultIndexURI                = "https://skillhub-1388575217.cos.ap-guangzhou.myqcloud.com/skills.json"
	defaultPrimaryDownloadTemplate = "https://skillhub-1388575217.cos.ap-guangzhou.myqcloud.com/skills/{slug}.zip"
	defaultSearchURL               = "https://api.skillhub.cn/api/v1/search"
	userAgent                      = "skillhub-cli/2026.3.3"
)

// Skill represents a skill entry in the index.
type Skill struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Summary     string `json:"summary"`
	Version     string `json:"version"`
	ZipURL      string `json:"zip_url"`
}

// SearchOptions allows configuration of search behavior.
type SearchOptions struct {
	// Limit maximum number of results (default 20)
	Limit int
	// RemoteSearchURL if empty, fallback to defaultSearchURL
	RemoteSearchURL string
	// IndexURI if empty, fallback to defaultIndexURI
	IndexURI string
}

// InstallOptions allows configuration of installation.
type InstallOptions struct {
	// Force overwrite existing directory
	Force bool
	// PrimaryDownloadURLTemplate supports {slug} placeholder
	PrimaryDownloadURLTemplate string
	// InstallDir destination root (default "./skills")
	InstallDir string
	// SearchURL used when skill not found in index
	SearchURL string
	// IndexURI fallback for skill lookup
	IndexURI string
}

// SearchResult is returned by SearchSkills.
type SearchResult struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

// SearchSkills searches for skills matching the query.
// It first tries local index, then falls back to remote search API.
// Returns a slice of SearchResult and an error if any.
func SearchSkills(query string, opts *SearchOptions) ([]SearchResult, error) {
	if opts == nil {
		opts = &SearchOptions{}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	indexURI := opts.IndexURI
	if indexURI == "" {
		indexURI = defaultIndexURI
	}
	remoteSearchURL := opts.RemoteSearchURL
	if remoteSearchURL == "" {
		remoteSearchURL = defaultSearchURL
	}

	query = strings.TrimSpace(strings.ToLower(query))

	// Try local index
	skills, err := fetchIndex(indexURI)
	if err == nil {
		matched := filterSkills(skills, query)
		if len(matched) > 0 {
			return convertToSearchResults(matched, limit), nil
		}
	}

	// Fallback to remote search
	remoteResults, err := fetchRemoteSearch(remoteSearchURL, query, limit)
	if err == nil && len(remoteResults) > 0 {
		return convertToSearchResults(remoteResults, limit), nil
	}

	return []SearchResult{}, nil
}

// InstallSkill downloads and installs a skill by its slug.
// It returns the absolute path where the skill was installed, or an error.
func InstallSkill(slug string, opts *InstallOptions) (string, error) {
	if opts == nil {
		opts = &InstallOptions{}
	}
	installDir := opts.InstallDir
	if installDir == "" {
		installDir = "./skills"
	}
	targetDir := filepath.Join(installDir, slug)

	primaryTemplate := opts.PrimaryDownloadURLTemplate
	if primaryTemplate == "" {
		primaryTemplate = defaultPrimaryDownloadTemplate
	}
	searchURL := opts.SearchURL
	if searchURL == "" {
		searchURL = defaultSearchURL
	}
	indexURI := opts.IndexURI
	if indexURI == "" {
		indexURI = defaultIndexURI
	}

	// Try to get skill info from index
	skills, err := fetchIndex(indexURI)
	var targetSkill *Skill
	if err == nil {
		for i := range skills {
			if skills[i].Slug == slug {
				targetSkill = &skills[i]
				break
			}
		}
	}

	// Fallback: remote search for exact slug
	if targetSkill == nil && searchURL != "" {
		remoteSkills, err := fetchRemoteSearch(searchURL, slug, 10)
		if err == nil {
			for i := range remoteSkills {
				if remoteSkills[i].Slug == slug {
					targetSkill = &remoteSkills[i]
					break
				}
			}
		}
	}

	// Determine download URL
	downloadURL := ""
	if targetSkill != nil && targetSkill.ZipURL != "" {
		downloadURL = targetSkill.ZipURL
	}
	if downloadURL == "" && primaryTemplate != "" {
		downloadURL = strings.ReplaceAll(primaryTemplate, "{slug}", slug)
	}
	if downloadURL == "" {
		return "", fmt.Errorf("no download URL found for skill %q", slug)
	}

	// Check target directory
	if _, err := os.Stat(targetDir); err == nil {
		if !opts.Force {
			return "", fmt.Errorf("target directory exists: %s (use Force=true to overwrite)", targetDir)
		}
		if err := os.RemoveAll(targetDir); err != nil {
			return "", fmt.Errorf("failed to remove existing directory: %w", err)
		}
	}

	// Download and extract
	tmpDir, err := os.MkdirTemp("", "skillhub-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, slug+".zip")
	if err := downloadFile(downloadURL, zipPath); err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}

	extractDir := filepath.Join(tmpDir, "extract")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create extract dir: %w", err)
	}
	if err := extractZip(zipPath, extractDir); err != nil {
		return "", fmt.Errorf("extraction failed: %w", err)
	}

	// Move extracted contents to target directory
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return "", fmt.Errorf("failed to create parent dir: %w", err)
	}
	if err := os.Rename(extractDir, targetDir); err != nil {
		// fallback copy
		if err := copyDir(extractDir, targetDir); err != nil {
			return "", fmt.Errorf("failed to move extracted content: %w", err)
		}
	}
	absPath, _ := filepath.Abs(targetDir)
	return absPath, nil
}

// ----------------------------------------------------------------------------
// Internal helper functions
// ----------------------------------------------------------------------------

func fetchIndex(uri string) ([]Skill, error) {
	data, err := readJSONFromURI(uri)
	if err != nil {
		return nil, err
	}
	var idx struct {
		Skills []Skill `json:"skills"`
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("invalid index JSON: %w", err)
	}
	return idx.Skills, nil
}

func filterSkills(skills []Skill, query string) []Skill {
	var results []Skill
	for _, s := range skills {
		text := strings.ToLower(s.Slug + " " + s.Name + " " + s.Description + " " + s.Summary)
		if strings.Contains(text, query) {
			results = append(results, s)
		}
	}
	// Simple ranking: sort by occurrence count (desc), then slug
	type scored struct {
		skill Skill
		score int
	}
	var scoredList []scored
	for _, s := range results {
		text := strings.ToLower(s.Slug + " " + s.Name + " " + s.Description + " " + s.Summary)
		score := strings.Count(text, query)
		scoredList = append(scoredList, scored{skill: s, score: score})
	}
	for i := 0; i < len(scoredList)-1; i++ {
		for j := i + 1; j < len(scoredList); j++ {
			if scoredList[i].score < scoredList[j].score ||
				(scoredList[i].score == scoredList[j].score && scoredList[i].skill.Slug > scoredList[j].skill.Slug) {
				scoredList[i], scoredList[j] = scoredList[j], scoredList[i]
			}
		}
	}
	final := make([]Skill, len(scoredList))
	for i, s := range scoredList {
		final[i] = s.skill
	}
	return final
}

func fetchRemoteSearch(searchURL, query string, limit int) ([]Skill, error) {
	if searchURL == "" {
		return nil, fmt.Errorf("no search URL provided")
	}
	params := url.Values{}
	params.Set("q", query)
	params.Set("limit", fmt.Sprintf("%d", limit))
	fullURL := searchURL
	if !strings.Contains(fullURL, "?") {
		fullURL += "?" + params.Encode()
	} else {
		fullURL += "&" + params.Encode()
	}
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search API returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var apiResp struct {
		Results []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"displayName"`
			Name        string `json:"name"`
			Summary     string `json:"summary"`
			Description string `json:"description"`
			Version     string `json:"version"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}
	var skills []Skill
	for _, r := range apiResp.Results {
		name := r.DisplayName
		if name == "" {
			name = r.Name
		}
		if name == "" {
			name = r.Slug
		}
		desc := r.Description
		if desc == "" {
			desc = r.Summary
		}
		skills = append(skills, Skill{
			Slug:        r.Slug,
			Name:        name,
			Description: desc,
			Version:     r.Version,
		})
	}
	return skills, nil
}

func convertToSearchResults(skills []Skill, limit int) []SearchResult {
	if len(skills) > limit {
		skills = skills[:limit]
	}
	results := make([]SearchResult, len(skills))
	for i, s := range skills {
		results[i] = SearchResult{
			Slug:        s.Slug,
			Name:        s.Name,
			Description: s.Description,
			Version:     s.Version,
		}
	}
	return results
}

func readJSONFromURI(uri string) ([]byte, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Scheme == "file" {
		// local file
		filePath := uri
		if parsed.Scheme == "file" {
			filePath = parsed.Path
			if parsed.Host != "" && parsed.Host != "localhost" {
				filePath = filepath.Join(parsed.Host, parsed.Path)
			}
		}
		filePath = filepath.FromSlash(filePath)
		return os.ReadFile(filePath)
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		req, err := http.NewRequest("GET", uri, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	return nil, fmt.Errorf("unsupported URI scheme: %s", parsed.Scheme)
}

func downloadFile(urlStr, destPath string) error {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return err
	}
	if parsed.Scheme == "file" {
		filePath := parsed.Path
		if parsed.Host != "" && parsed.Host != "localhost" {
			filePath = filepath.Join(parsed.Host, parsed.Path)
		}
		filePath = filepath.FromSlash(filePath)
		src, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := os.Create(destPath)
		if err != nil {
			return err
		}
		defer dst.Close()
		_, err = io.Copy(dst, src)
		return err
	}
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		fpath := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, f.Mode())
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(dest, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, info.Mode())
	})
}

// ----------------------------------------------------------------------------
// Example usage in main()
// ----------------------------------------------------------------------------
func main() {
	// Example: search for "python" skills
	fmt.Println("=== Searching for 'python' ===")
	results, err := SearchSkills("wasm", nil)
	if err != nil {
		fmt.Printf("Search error: %v\n", err)
	} else {
		for _, r := range results {
			fmt.Printf("%s\n", r.Slug)
		}
	}

	// Example: install a specific skill (e.g., "find-skills")
	fmt.Println("\n=== Installing 'find-skills' ===")
	path, err := InstallSkill("boxed-ffmpeg", &InstallOptions{
		Force:      true,
		InstallDir: "./my-skills",
	})
	if err != nil {
		fmt.Printf("Install error: %v\n", err)
	} else {
		fmt.Printf("Installed to: %s\n", path)
	}
}
