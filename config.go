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

type yamlConfig struct {
	// Token fields — optional. Environment variables take priority.
	DiscordToken   string `yaml:"discord_token"`
	DiscordGuildID string `yaml:"discord_guild_id"`
	AnthropicKey   string `yaml:"anthropic_api_key"`

	Admin         AdminConfig `yaml:"admin"`
	OCRWorkers    int         `yaml:"ocr_workers"`     // parallel OCR goroutines (default 4)
	ScanOnStartup bool        `yaml:"scan_on_startup"` // scan PDF dir on launch (default false)
}

type Config struct {
	DiscordToken   string
	DiscordGuildID string
	AnthropicKey   string
	Admin          AdminConfig
	DBPath         string
	PDFDir         string
	OCRWorkers     int
	ScanOnStartup  bool
}

// LoadConfig loads configuration from the data directory.
//   - Secrets  → <dataDir>/.env  (or environment variables)
//   - Settings → <dataDir>/config.yaml
//
// Token precedence (highest to lowest):
//  1. Environment variable (DISCORD_TOKEN, DISCORD_GUILD_ID, ANTHROPIC_API_KEY)
//  2. <dataDir>/.env file
//  3. config.yaml (discord_token, discord_guild_id, anthropic_api_key fields)
//
// DB and PDF paths default to subdirectories of dataDir.
func LoadConfig(dataDir string) Config {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}

	// Load secrets from .env (ignore error — env vars may already be set)
	_ = godotenv.Load(filepath.Join(dataDir, ".env"))

	// Load settings from config.yaml
	yc := loadYAMLConfig(filepath.Join(dataDir, "config.yaml"))

	// Env vars take priority over config.yaml values
	ocr := yc.OCRWorkers
	if ocr <= 0 {
		ocr = 4
	}
	cfg := Config{
		DiscordToken:   firstNonEmpty(os.Getenv("DISCORD_TOKEN"), yc.DiscordToken),
		DiscordGuildID: firstNonEmpty(os.Getenv("DISCORD_GUILD_ID"), yc.DiscordGuildID),
		AnthropicKey:   firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), yc.AnthropicKey),
		Admin:          yc.Admin,
		DBPath:         getEnvDefault("DB_PATH", filepath.Join(dataDir, "rules.db")),
		PDFDir:         getEnvDefault("PDF_DIR", filepath.Join(dataDir, "pdfs")),
		OCRWorkers:     ocr,
		ScanOnStartup:  yc.ScanOnStartup,
	}

	if cfg.DiscordToken == "" {
		log.Fatal("DISCORD_TOKEN is required (set DISCORD_TOKEN env var, add to <data-dir>/.env, or set discord_token in config.yaml)")
	}
	if cfg.AnthropicKey == "" {
		log.Fatal("ANTHROPIC_API_KEY is required (set ANTHROPIC_API_KEY env var, add to <data-dir>/.env, or set anthropic_api_key in config.yaml)")
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
# Edit this file to configure the bot.

# Bot tokens — can also be set via environment variables or the .env file.
# Environment variables and .env values take priority over what is set here.
#
# discord_token: ""         # overrides DISCORD_TOKEN env var
# discord_guild_id: ""      # overrides DISCORD_GUILD_ID env var (empty = global slash commands)
# anthropic_api_key: ""     # overrides ANTHROPIC_API_KEY env var

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

# OCR worker threads used when indexing scanned PDFs.
# Higher values speed up indexing at the cost of more CPU/RAM.
# Default: 4
ocr_workers: 4

# Scan the PDF directory for new books on startup.
# When true, any PDF in the pdfs/ folder that is not yet indexed will be
# indexed automatically before the bot connects to Discord.
# Default: false
scan_on_startup: false
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

// firstNonEmpty returns the first non-empty string from vals.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
