// Command standvirtual-notifier is a long-lived daemon that watches
// Standvirtual.com car searches and posts new listings and price changes into a
// Matrix room. Searches are managed at runtime with !sv commands in that room.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/config"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/matrix"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/poller"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/standvirtual"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	bot, err := matrix.New(cfg, st)
	if err != nil {
		log.Fatalf("matrix: %v", err)
	}

	p := poller.New(st, standvirtual.NewClient(), bot,
		time.Duration(cfg.Poll.IntervalSeconds)*time.Second, cfg.Poll.MaxPages)
	bot.SetSeeder(p)

	// Cancel everything on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go p.Run(ctx)

	log.Printf("standvirtual-notifier started (interval=%ds, db=%s)", cfg.Poll.IntervalSeconds, cfg.DBPath)
	if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("matrix sync: %v", err)
	}
	log.Println("shutting down")
}
