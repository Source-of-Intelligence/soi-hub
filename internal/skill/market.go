// Package skill provides market search and source management for skills.
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	skillpkg "github.com/Source-of-Intelligence/soi-hub/pkg/skill"
)

// Market manages skill sources and online search.
type Market struct {
	sources []*skillpkg.Source
	mu      sync.RWMutex
	client  *http.Client
}

// NewMarket creates a market with default sources.
func NewMarket() *Market {
	m := &Market{
		sources: make([]*skillpkg.Source, 0),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
	m.initDefaults()
	return m
}

func (mkt *Market) initDefaults() {
	mkt.sources = append(mkt.sources, &skillpkg.Source{
		Name: "official", URL: "https://raw.githubusercontent.com/soi-dev/skills/main/index.json",
		Type: "market", Enabled: true, Priority: 10,
	})
}

// ListSources returns all configured skill sources.
func (mkt *Market) ListSources() []*skillpkg.Source {
	mkt.mu.RLock()
	defer mkt.mu.RUnlock()
	result := make([]*skillpkg.Source, len(mkt.sources))
	copy(result, mkt.sources)
	return result
}

// AddSource adds a new skill source.
func (mkt *Market) AddSource(src *skillpkg.Source) {
	mkt.mu.Lock()
	defer mkt.mu.Unlock()
	mkt.sources = append(mkt.sources, src)
}

// RemoveSource removes a skill source by name.
func (mkt *Market) RemoveSource(name string) bool {
	mkt.mu.Lock()
	defer mkt.mu.Unlock()
	for i, s := range mkt.sources {
		if s.Name == name {
			mkt.sources = append(mkt.sources[:i], mkt.sources[i+1:]...)
			return true
		}
	}
	return false
}

// Search searches all enabled sources concurrently for skills matching the query.
func (mkt *Market) Search(ctx context.Context, query string) ([]*skillpkg.SearchResult, error) {
	mkt.mu.RLock()
	sources := make([]*skillpkg.Source, 0)
	for _, s := range mkt.sources {
		if s.Enabled {
			sources = append(sources, s)
		}
	}
	mkt.mu.RUnlock()
	if len(sources) == 0 {
		return nil, fmt.Errorf("no enabled skill sources configured")
	}
	type result struct {
		src  string
		data []*skillpkg.SearchResult
		err  error
	}
	ch := make(chan result, len(sources))
	for _, src := range sources {
		go func(s *skillpkg.Source) {
			results, err := mkt.searchSource(ctx, s, query)
			ch <- result{src: s.Name, data: results, err: err}
		}(src)
	}
	var allResults []*skillpkg.SearchResult
	for range sources {
		r := <-ch
		if r.err != nil {
			continue
		}
		allResults = append(allResults, r.data...)
	}
	return allResults, nil
}

func (mkt *Market) searchSource(ctx context.Context, src *skillpkg.Source, query string) ([]*skillpkg.SearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", src.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := mkt.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("source returned %d", resp.StatusCode)
	}
	var index struct {
		Skills []*skillpkg.SearchResult `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		return nil, err
	}
	if query == "" {
		return index.Skills, nil
	}
	var filtered []*skillpkg.SearchResult
	for _, s := range index.Skills {
		if matchQuery(s, query) {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

func matchQuery(s *skillpkg.SearchResult, query string) bool {
	return containsFold(s.Name, query) || containsFold(s.Description, query) || containsAnyFold(s.Tags, query)
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (s[:len(sub)] == sub || containsSubstringFold(s, sub))
}

func containsSubstringFold(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func containsAnyFold(tags []string, query string) bool {
	for _, t := range tags {
		if containsFold(t, query) {
			return true
		}
	}
	return false
}
