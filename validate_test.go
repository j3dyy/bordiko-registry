package main

import (
	"context"
	"encoding/base64"
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

func TestValidateAssets(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...) // valid PNG signature

	out, err := validateAssets(map[string]string{"role-good.png": b64(png)})
	if err != nil || len(out) != 1 {
		t.Fatalf("a valid PNG should be accepted: %v", err)
	}
	// SVG / anything script-bearing is rejected (type sniffed from bytes).
	if _, err := validateAssets(map[string]string{"x.png": b64([]byte(`<?xml version="1.0"?><svg onload="alert(1)"/>`))}); err == nil {
		t.Fatal("an SVG/text asset must be rejected")
	}
	// path-traversal ids rejected.
	if _, err := validateAssets(map[string]string{"../secret": b64(png)}); err == nil {
		t.Fatal("a path-y asset id must be rejected")
	}
	// oversized rejected.
	big := b64(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, maxAssetBytes+1)...))
	if _, err := validateAssets(map[string]string{"big.png": big}); err == nil {
		t.Fatal("an oversized asset must be rejected")
	}
	// nil is fine (no assets).
	if out, err := validateAssets(nil); err != nil || out != nil {
		t.Fatalf("no assets should be a no-op: %v", err)
	}
}
