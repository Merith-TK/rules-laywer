package main

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	DiscordToken   string
	DiscordGuildID string // optional: for instant slash command registration
	AnthropicKey   string
	AdminRoleName  string
	DBPath         string
	PDFDir         string
}

func LoadConfig() Config {
	// Load .env if present; ignore error (env vars may already be set)
	_ = godotenv.Load()

	cfg := Config{
		DiscordToken:   os.Getenv("DISCORD_TOKEN"),
		DiscordGuildID: os.Getenv("DISCORD_GUILD_ID"),
		AnthropicKey:   os.Getenv("ANTHROPIC_API_KEY"),
		AdminRoleName:  getEnvDefault("ADMIN_ROLE_NAME", "DM"),
		DBPath:         getEnvDefault("DB_PATH", "./rules.db"),
		PDFDir:         getEnvDefault("PDF_DIR", "./pdfs"),
	}

	if cfg.DiscordToken == "" {
		log.Fatal("DISCORD_TOKEN is required")
	}
	if cfg.AnthropicKey == "" {
		log.Fatal("ANTHROPIC_API_KEY is required")
	}

	return cfg
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
