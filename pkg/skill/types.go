// Package skill provides skill specification types for the skill market.
package skill

import (
	"time"
)

// Manifest represents a skill.yaml manifest file.
type Manifest struct {
	// API version
	APIVersion string `yaml:"apiVersion"`
	// Resource kind
	Kind string `yaml:"kind"`

	// Metadata
	Metadata Metadata `yaml:"metadata"`

	// Specification
	Spec Spec `yaml:"spec"`
}

// Metadata contains skill metadata.
type Metadata struct {
	Name        string            `yaml:"name"`
	Version     string            `yaml:"version"`
	Author      string            `yaml:"author,omitempty"`
	Description string            `yaml:"description"`
	Tags        []string          `yaml:"tags,omitempty"`
	Icon        string            `yaml:"icon,omitempty"`
	License     string            `yaml:"license,omitempty"`
	Homepage    string            `yaml:"homepage,omitempty"`
	Repository  string            `yaml:"repository,omitempty"`
	CreatedAt   time.Time         `yaml:"createdAt,omitempty"`
	UpdatedAt   time.Time         `yaml:"updatedAt,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
}

// Spec contains skill specification.
type Spec struct {
	// Runtime configuration
	Runtime Runtime `yaml:"runtime"`

	// Dependencies
	Requires []Dependency `yaml:"requires,omitempty"`

	// Provided capabilities
	Provides Provides `yaml:"provides"`

	// Configuration options
	Config []ConfigOption `yaml:"config,omitempty"`

	// Permission declarations
	Permissions Permissions `yaml:"permissions,omitempty"`
}

// Trigger defines trigger configuration at provides level.
type Trigger struct {
	Keywords []string `yaml:"keywords,omitempty"`
	Prefix   string   `yaml:"prefix,omitempty"`
	Regex    string   `yaml:"regex,omitempty"`
	Events   []string `yaml:"events,omitempty"`
	Priority int      `yaml:"priority,omitempty"`
}

// Runtime configuration.
type Runtime struct {
	// Runtime type: "builtin", "go", "wasm", "python", "js", "soi"
	Type string `yaml:"type"`
	// Entry file (for go/wasm/python/js/soi)
	Entry string `yaml:"entry,omitempty"`
	// Main function/struct name
	Main string `yaml:"main,omitempty"`
	// WASM-specific configuration
	Wasm *RuntimeWasmConfig `yaml:"wasm,omitempty"`
	// Sandbox capabilities (for SOI plugins)
	Uses []string `yaml:"uses,omitempty"`
}

// RuntimeWasmConfig is the WASM runtime configuration.
type RuntimeWasmConfig struct {
	Sandbox        RuntimeWasmSandbox `yaml:"sandbox,omitempty"`
	Memory         RuntimeWasmMemory  `yaml:"memory,omitempty"`
	Timeout        string             `yaml:"timeout,omitempty"` // e.g. "30s"
	MaxConcurrency int                `yaml:"maxConcurrency,omitempty"`
	AllowedHosts   []string           `yaml:"allowedHosts,omitempty"`
}

type RuntimeWasmSandbox struct {
	Subdir string `yaml:"subdir,omitempty"`
}

type RuntimeWasmMemory struct {
	Initial uint32 `yaml:"initial,omitempty"`
	Maximum uint32 `yaml:"maximum,omitempty"`
}

// Dependency declaration.
type Dependency struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// Provides declares what the skill offers.
type Provides struct {
	// Tools provided by this skill
	Tools []ToolDef `yaml:"tools,omitempty"`
	// Trigger configuration at provides level
	Triggers *Trigger `yaml:"triggers,omitempty"`
	// Prompts provided by this skill
	Prompts []PromptDef `yaml:"prompts,omitempty"`
	// Resources provided by this skill
	Resources []ResourceDef `yaml:"resources,omitempty"`
	// Instructions for skill execution (legacy compatibility)
	Instructions string `yaml:"instructions,omitempty"`
}

// ToolDef defines a tool provided by a skill.
type ToolDef struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Parameters  []ParamDef    `yaml:"parameters,omitempty"`
	Returns     string        `yaml:"returns,omitempty"`
	Examples    []ToolExample `yaml:"examples,omitempty"`
}

// ParamDef defines a tool parameter.
type ParamDef struct {
	Name        string      `yaml:"name"`
	Type        string      `yaml:"type"` // "string", "number", "boolean", "array", "object"
	Required    bool        `yaml:"required"`
	Default     interface{} `yaml:"default,omitempty"`
	Description string      `yaml:"description,omitempty"`
	Enum        []string    `yaml:"enum,omitempty"`
}

// ToolExample provides usage example.
type ToolExample struct {
	Input  map[string]interface{} `yaml:"input"`
	Output string                 `yaml:"output"`
}

// PromptDef defines a prompt template.
type PromptDef struct {
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Template    string           `yaml:"template"`
	Variables   []PromptVariable `yaml:"variables,omitempty"`
}

// PromptVariable defines a template variable.
type PromptVariable struct {
	Name        string      `yaml:"name"`
	Type        string      `yaml:"type"`
	Default     interface{} `yaml:"default,omitempty"`
	Description string      `yaml:"description,omitempty"`
}

// ResourceDef defines a resource provided by a skill.
type ResourceDef struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // "file", "memory", "session", "custom"
	Description string `yaml:"description"`
	Path        string `yaml:"path,omitempty"`
}

// ConfigOption defines a configuration option.
type ConfigOption struct {
	Name        string      `yaml:"name"`
	Type        string      `yaml:"type"` // "string", "number", "boolean", "enum"
	Default     interface{} `yaml:"default,omitempty"`
	Description string      `yaml:"description,omitempty"`
	Values      []string    `yaml:"values,omitempty"` // for enum type
	Required    bool        `yaml:"required,omitempty"`
	Secret      bool        `yaml:"secret,omitempty"` // marks as sensitive
}

// Permissions declares what permissions the skill needs.
type Permissions struct {
	Filesystem []string `yaml:"filesystem,omitempty"` // "read", "write", "delete"
	Network    []string `yaml:"network,omitempty"`    // domains allowed
	Exec       []string `yaml:"exec,omitempty"`       // commands allowed
	Env        []string `yaml:"env,omitempty"`        // env vars allowed
}

// SkillInfo contains runtime information about a loaded skill.
type SkillInfo struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Description  string            `json:"description"`
	Author       string            `json:"author,omitempty"`
	Status       SkillStatus       `json:"status"` // "active", "inactive", "error"
	Enabled      bool              `json:"enabled"`
	Builtin      bool              `json:"builtin"` // true for built-in skills
	InstallTime  time.Time         `json:"installTime,omitempty"`
	LastUsed     time.Time         `json:"lastUsed,omitempty"`
	UsageCount   int               `json:"usageCount"`
	Rating       float64           `json:"rating"`
	Source       string            `json:"source"` // "builtin", "local", "market", "git"
	Path         string            `json:"path"`   // installation path
	Dependencies []string          `json:"dependencies,omitempty"`
	Tools        []string          `json:"tools,omitempty"`
	Prompts      []string          `json:"prompts,omitempty"`
	Triggers     []string          `json:"triggers,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// SkillStatus represents skill runtime status.
type SkillStatus string

const (
	SkillStatusActive   SkillStatus = "active"
	SkillStatusInactive SkillStatus = "inactive"
	SkillStatusError    SkillStatus = "error"
	SkillStatusLoading  SkillStatus = "loading"
)

// InstallOptions for skill installation.
type InstallOptions struct {
	Source   string                 // "local", "market", "git", "url"
	URL      string                 // URL for remote sources
	Version  string                 // specific version to install
	Force    bool                   // force reinstall
	SkipDeps bool                   // skip dependency resolution
	Config   map[string]interface{} // initial configuration
}

// Filter for skill listing/searching.
type Filter struct {
	NameContains string
	Tags         []string
	Author       string
	Status       SkillStatus
	OnlyEnabled  bool
	OnlyBuiltin  bool
	Source       string
}

// Source represents a skill repository source.
type Source struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Type     string `json:"type"` // "local", "git", "market", "http"
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"`
}

// SearchResult from skill market search.
type SearchResult struct {
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Description   string   `json:"description"`
	Author        string   `json:"author"`
	Downloads     int      `json:"downloads"`
	Rating        float64  `json:"rating"`
	Tags          []string `json:"tags"`
	Source        string   `json:"source"`
	InstallURL    string   `json:"installUrl"`
	Compatibility string   `json:"compatibility"` // "compatible", "partial", "incompatible"
}
