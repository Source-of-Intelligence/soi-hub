package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// config is the top-level configuration for the skill-market service.
type config struct {
	Server  ServerConfig  `mapstructure:"server"`
	Storage StorageConfig `mapstructure:"storage"`
	Market  MarketConfig  `mapstructure:"market"`
}

type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

type StorageConfig struct {
	DB        string `mapstructure:"db"`
	SkillsDir string `mapstructure:"skills_dir"`
}

type MarketConfig struct {
	Sources []MarketSourceConfig `mapstructure:"sources"`
}

type MarketSourceConfig struct {
	Name     string `mapstructure:"name"`
	URL      string `mapstructure:"url"`
	Type     string `mapstructure:"type"`
	Enabled  bool   `mapstructure:"enabled"`
	Priority int    `mapstructure:"priority"`
}

func defaultConfig() *config {
	return &config{
		Server:  ServerConfig{Host: "0.0.0.0", Port: 9090},
		Storage: StorageConfig{DB: "./data/sm.db", SkillsDir: "./skills"},
		Market: MarketConfig{
			Sources: []MarketSourceConfig{{Name: "official", URL: "https://raw.githubusercontent.com/soi-dev/skills/main/index.json", Type: "http", Enabled: true, Priority: 10}},
		},
	}
}

func loadConfig(configPath string) (*config, error) {
	cfg := defaultConfig()
	if configPath == "" {
		configPath = findConfigFile()
	}
	if configPath != "" {
		v := viper.New()
		v.SetConfigFile(configPath)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("failed to read config %q: %w", configPath, err)
		}
		if err := v.Unmarshal(cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config: %w", err)
		}
	}
	return cfg, nil
}

func findConfigFile() string {
	for _, p := range []string{"./skill-market.yaml.template", "./skill-market.yml", "./configs/skill-market.yaml.template", "./configs/skill-market.yml", filepath.Join(os.ExpandEnv("$HOME"), ".soi", "skill-market.yaml.template")} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func (c *config) applyCLI(host string, port int, skillsDir string, dbDSN string) {
	if host != "" {
		c.Server.Host = host
	}
	if port > 0 {
		c.Server.Port = port
	}
	if skillsDir != "" {
		c.Storage.SkillsDir = skillsDir
	}
	if dbDSN != "" {
		c.Storage.DB = dbDSN
	}
}

func (c *config) applyEnv() {
	if v := os.Getenv("SKILL_MARKET_HOST"); v != "" {
		c.Server.Host = v
	}
	if v := os.Getenv("SKILL_MARKET_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			c.Server.Port = port
		}
	}
	if v := os.Getenv("SKILL_MARKET_SKILLS_DIR"); v != "" {
		c.Storage.SkillsDir = v
	}
	if v := os.Getenv("SKILL_MARKET_DB"); v != "" {
		c.Storage.DB = v
	}
}

func (c *config) absSkillsDir() string {
	abs, _ := filepath.Abs(c.Storage.SkillsDir)
	return abs
}

func (c *config) listenAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

func (c *config) validate() error {
	if c.Storage.DB == "" {
		return fmt.Errorf("storage.db is required")
	}
	if c.Storage.SkillsDir == "" {
		return fmt.Errorf("storage.skills_dir is required")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	return nil
}
