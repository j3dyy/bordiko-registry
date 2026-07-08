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
//	REGISTRY_DATA_DIR            persist packages here    (default: in-memory)
//	REGISTRY_REQUIRE_MODERATION  if "true", uploads land as "pending"
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	dir := os.Getenv("REGISTRY_DATA_DIR")
	store, err := NewStore(dir, time.Now)
	if err != nil {
		log.Fatalf("registry store: %v", err)
	}
	defer store.Close()
	if dir != "" {
		log.Printf("registry persisting to %q (%d game(s) loaded)", dir, len(store.ListLatest()))
	} else {
		log.Printf("registry in-memory (set REGISTRY_DATA_DIR to persist)")
	}

	srv := NewServer(store, os.Getenv("REGISTRY_REQUIRE_MODERATION") == "true", time.Now)
	addr := env("REGISTRY_ADDR", ":8082")
	log.Printf("bordiko registry listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
		log.Fatalf("registry failed: %v", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
