package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func manifestFor(gameID, board string, min, max int) json.RawMessage {
	m, _ := json.Marshal(map[string]any{
		"schema":      1,
		"gameId":      gameID,
		"version":     "1.0.0",
		"displayName": gameID,
		"players":     map[string]int{"min": min, "max": max},
		"board":       board,
		"artifacts":   map[string]string{"wasm": "", "ui": ""},
	})
	return m
}

func TestValidateAcceptsRealGame(t *testing.T) {
	wasm, err := os.ReadFile("../../dist/eights.wasm")
	if err != nil {
		t.Skipf("dist/eights.wasm not built: %v", err)
	}
	m, sha, err := validatePackage(context.Background(), manifestFor("eights", "tableau", 2, 4), wasm)
	if err != nil {
		t.Fatalf("expected valid package, got: %v", err)
	}
	if m.GameID != "eights" || sha == "" {
		t.Fatalf("unexpected result: %+v sha=%s", m, sha)
	}
}

func TestValidateRejectsForbiddenImport(t *testing.T) {
	// A minimal, hand-encoded wasm module that imports a forbidden host function
	// env.bad — exactly the kind of thing the allow-list must stop.
	bad := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // header
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type section: () -> ()
		0x02, 0x0b, 0x01, // import section, 1 import
		0x03, 'e', 'n', 'v', // module "env"
		0x03, 'b', 'a', 'd', // field "bad"
		0x00, 0x00, // kind=func, typeidx=0
	}
	_, _, err := validatePackage(context.Background(), manifestFor("evil", "grid", 2, 2), bad)
	if err == nil {
		t.Fatal("expected a forbidden-import rejection, got nil")
	}
}

func TestValidateRejectsBadManifest(t *testing.T) {
	wasm, err := os.ReadFile("../../dist/eights.wasm")
	if err != nil {
		t.Skipf("dist/eights.wasm not built: %v", err)
	}
	// gameId with spaces is invalid.
	bad := manifestFor("Bad Name", "tableau", 2, 4)
	if _, _, err := validatePackage(context.Background(), bad, wasm); err == nil {
		t.Fatal("expected manifest rejection")
	}
}
