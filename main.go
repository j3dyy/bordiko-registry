// Command registry is the marketplace service of Bordiko.
//
// Developers publish game packages (manifest + game.wasm) here. Every upload is
// validated before it can be played: the manifest must be well-formed, the wasm
// may only import the WASI stdio surface (no host I/O), and a setup scenario
// must run to completion under memory + time limits. Published games are served
// to the game-host on demand, so a new game becomes playable without a redeploy.
//
// Config (env):
//
//	REGISTRY_ADDR                listen address           (default ":8082")
//	DATABASE_URL                 Postgres DSN; wasm stored in bytea (durable)
//	REGISTRY_DATA_DIR            persist packages to disk (used when no DATABASE_URL)
//	REGISTRY_REQUIRE_MODERATION  if "true", uploads land as "pending"
//
// Prefer DATABASE_URL in production: the filesystem store loses published games
// on ephemeral redeploys, whereas Postgres bytea survives them.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		log.Fatalf("registry store: %v", err)
	}
	defer store.Close()

	srv := NewServer(store, os.Getenv("REGISTRY_REQUIRE_MODERATION") == "true", time.Now)
	addr := env("REGISTRY_ADDR", ":8082")
	log.Printf("bordiko registry listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
		log.Fatalf("registry failed: %v", err)
	}
}

// openStore selects the durable Postgres store when DATABASE_URL is set,
// otherwise the filesystem/in-memory LocalStore.
func openStore(ctx context.Context) (Store, error) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		log.Printf("registry using Postgres store (wasm in bytea, durable)")
		s, err := NewPostgresStore(ctx, url)
		if err != nil {
			return nil, err
		}
		log.Printf("registry connected to Postgres (%d game(s) in catalog)", len(s.ListLatest()))
		return s, nil
	}
	dir := os.Getenv("REGISTRY_DATA_DIR")
	s, err := NewLocalStore(dir, time.Now)
	if err != nil {
		return nil, err
	}
	if dir != "" {
		log.Printf("registry persisting to %q (%d game(s) loaded) — set DATABASE_URL for durable storage", dir, len(s.ListLatest()))
	} else {
		log.Printf("registry in-memory (set DATABASE_URL or REGISTRY_DATA_DIR to persist)")
	}
	return s, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
