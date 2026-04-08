package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// AdminConfig controls who can manage books (upload/remove/scan).
// Role names and IDs are checked against the user's Discord roles.
// User IDs grant admin access to specific users regardless of role.
type AdminConfig struct {
	RoleNames []string `yaml:"role_names"` // matched case-insensitively
	RoleIDs   []string `yaml:"role_ids"`   // Discord snowflake IDs (more reliable)
	UserIDs   []string `yaml:"user_ids"`   // specific Discord user snowflake IDs
}

// TokensConfig holds API tokens that may be set in config.yaml instead of
// (or in addition to) environment variables. Environment variables always
// take precedence over values defined here.
type TokensConfig struct {
	DiscordToken   string `yaml:"discord_token"`
	DiscordGuildID string `yaml:"discord_guild_id"`
	AnthropicKey   string `yaml:"anthropic_api_key"`
}

type yamlConfig struct {
	Tokens TokensConfig `yaml:"tokens"`
	Admin  AdminConfig  `yaml:"admin"`
}

type Config struct {
	DiscordToken   string
	DiscordGuildID string
	AnthropicKey   string
	Admin          AdminConfig
	DBPath         string
	PDFDir         string
}

// LoadConfig loads configuration from the data directory.
//   - Secrets  → <dataDir>/.env  (loaded into environment before reading)
//   - Settings → <dataDir>/config.yaml  (tokens + admin)
//
// Resolution order for tokens (highest priority first):
//  1. Environment variable (e.g. DISCORD_TOKEN)
//  2. config.yaml `tokens` section
//
// DB and PDF paths default to subdirectories of dataDir.
func LoadConfig(dataDir string) Config {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}

	// Load secrets from .env (ignore error — env vars may already be set)
	_ = godotenv.Load(filepath.Join(dataDir, ".env"))

	// Load settings from config.yaml
	yamlCfg := loadYAMLConfig(filepath.Join(dataDir, "config.yaml"))

	// Resolve each token: env var wins, yaml value is the fallback.
	cfg := Config{
		DiscordToken:   firstNonEmpty(os.Getenv("DISCORD_TOKEN"), yamlCfg.Tokens.DiscordToken),
		DiscordGuildID: firstNonEmpty(os.Getenv("DISCORD_GUILD_ID"), yamlCfg.Tokens.DiscordGuildID),
		AnthropicKey:   firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), yamlCfg.Tokens.AnthropicKey),
		Admin:          yamlCfg.Admin,
		DBPath:         getEnvDefault("DB_PATH", filepath.Join(dataDir, "rules.db")),
		PDFDir:         getEnvDefault("PDF_DIR", filepath.Join(dataDir, "pdfs")),
	}

	if cfg.DiscordToken == "" {
		log.Fatal("DISCORD_TOKEN is required (set DISCORD_TOKEN env var, <data-dir>/.env, or tokens.discord_token in config.yaml)")
	}
	if cfg.AnthropicKey == "" {
		log.Fatal("ANTHROPIC_API_KEY is required (set ANTHROPIC_API_KEY env var, <data-dir>/.env, or tokens.anthropic_api_key in config.yaml)")
	}

	return cfg
}

// loadYAMLConfig reads config.yaml. If the file doesn't exist it writes a
// default one and returns the default config so the bot still starts.
func loadYAMLConfig(path string) yamlConfig {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		writeDefaultYAML(path)
		return yamlConfig{Admin: defaultAdminConfig()}
	}
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}

	var yc yamlConfig
	if err := yaml.Unmarshal(data, &yc); err != nil {
		log.Fatalf("parse %s: %v", path, err)
	}

	// If the file exists but admin section is empty, use defaults
	if len(yc.Admin.RoleNames) == 0 && len(yc.Admin.RoleIDs) == 0 && len(yc.Admin.UserIDs) == 0 {
		yc.Admin = defaultAdminConfig()
	}
	return yc
}

func defaultAdminConfig() AdminConfig {
	return AdminConfig{
		RoleNames: []string{"DM"},
	}
}

func writeDefaultYAML(path string) {
	const defaultContent = `# Rules Lawyer — bot configuration

# API tokens can optionally be set here instead of (or in addition to) environment
# variables. Environment variables always take priority over values defined below.
#
# tokens:
#   discord_token: your_discord_bot_token_here
#   discord_guild_id: ""          # optional: guild ID for instant command registration
#   anthropic_api_key: your_anthropic_api_key_here

# Controls who can manage books (upload/remove/scan).
admin:
  # Role names (case-insensitive). Users with any of these roles can
  # upload, remove, and scan books.
  role_names:
    - DM

  # Role IDs (Discord snowflakes). More reliable than names since they
  # don't break if a role is renamed. Right-click a role → Copy Role ID.
  role_ids: []

  # User IDs. These specific users always have admin access regardless
  # of their roles. Right-click a user → Copy User ID.
  user_ids: []
`
	if err := os.WriteFile(path, []byte(defaultContent), 0644); err != nil {
		log.Printf("warn: could not write default config.yaml: %v", err)
	} else {
		log.Printf("created default config at %s — edit it to configure admins", path)
	}
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// firstNonEmpty returns the first non-empty string from the provided values.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
