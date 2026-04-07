package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"rules-laywer/bot"
	"rules-laywer/indexer"
	"rules-laywer/store"
)

func main() {
	cfg := LoadConfig()

	// Open SQLite store
	s, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	// Ensure PDF directory exists
	if err := os.MkdirAll(cfg.PDFDir, 0755); err != nil {
		log.Printf("warn: could not create PDF dir %s: %v", cfg.PDFDir, err)
	}

	// Scan PDF directory for any un-indexed books on startup
	added, errs := indexer.ScanDir(cfg.PDFDir, s)
	for _, name := range added {
		log.Printf("startup: indexed %s", name)
	}
	for _, e := range errs {
		log.Printf("startup warn: %v", e)
	}

	// Start the Discord bot
	b, err := bot.New(
		cfg.DiscordToken,
		cfg.AnthropicKey,
		cfg.AdminRoleName,
		cfg.PDFDir,
		cfg.DiscordGuildID,
		s,
	)
	if err != nil {
		log.Fatalf("create bot: %v", err)
	}

	if err := b.Open(); err != nil {
		log.Fatalf("open discord connection: %v", err)
	}
	defer b.Close()

	log.Println("Rules Lawyer is running. Press Ctrl+C to exit.")

	// Wait for termination signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down...")
}
