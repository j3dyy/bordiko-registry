package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// Asset limits (Option 1c). Assets are the developer's own images for the
// declarative board — kept small, raster-only, and path-safe.
const (
	maxAssetBytes = 256 << 10 // 256 KiB each
	maxAssetTotal = 4 << 20   // 4 MiB total
	maxAssetCount = 32
)

var (
	assetIDRe         = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	allowedAssetTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}
)

// validateAssets decodes and vets the uploaded images: base64, a safe id, a size
// cap, and a real raster type (SVG and anything script-bearing is rejected — the
// content type is sniffed from the bytes, not trusted from the client).
func validateAssets(raw map[string]string) (map[string][]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxAssetCount {
		return nil, fmt.Errorf("too many assets: %d (max %d)", len(raw), maxAssetCount)
	}
	out := make(map[string][]byte, len(raw))
	total := 0
	for id, b64 := range raw {
		if !assetIDRe.MatchString(id) {
			return nil, fmt.Errorf("invalid asset id %q (lowercase, no path separators)", id)
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("asset %q is not valid base64", id)
		}
		if len(data) == 0 || len(data) > maxAssetBytes {
			return nil, fmt.Errorf("asset %q is %d bytes (max %d)", id, len(data), maxAssetBytes)
		}
		ct := http.DetectContentType(data)
		if !allowedAssetTypes[ct] {
			return nil, fmt.Errorf("asset %q is %s — only PNG/JPEG/GIF/WebP images are allowed", id, ct)
		}
		total += len(data)
		if total > maxAssetTotal {
			return nil, fmt.Errorf("assets exceed the %d-byte total cap", maxAssetTotal)
		}
		out[id] = data
	}
	return out, nil
}

// Uploading arbitrary code is the whole point of an open marketplace, so the
// registry never trusts a package. Three gates run before anything is published:
//   1. the manifest is well-formed and matches the wasm,
//   2. the wasm only imports the WASI stdio surface — no host functions that
//      could do I/O (this is what the runtime sandbox already relies on), and
//   3. the wasm actually implements the guest contract (a setup scenario runs
//      to completion under memory + time limits and returns valid state).

const (
	maxWasmBytes    = 24 << 20 // 24 MiB
	validateMemPage = 1024     // 64 MiB
	validateTimeout = 5 * time.Second
)

// The only import module a game is allowed to reference. Everything a game needs
// (deterministic RNG, clock) is provided by the engine in userland; the guest's
// only host surface is WASI stdin/stdout.
var allowedImportModules = map[string]bool{"wasi_snapshot_preview1": true}

var (
	gameIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	boards    = map[string]bool{"grid": true, "hex": true, "network": true, "tableau": true, "custom": true}
)

type Manifest struct {
	Schema      int    `json:"schema"`
	GameID      string `json:"gameId"`
	Version     string `json:"version"`
	DisplayName string `json:"displayName"`
	Players     struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"players"`
	Board     string `json:"board"`
	Artifacts struct {
		Wasm string `json:"wasm"`
		UI   string `json:"ui"`
	} `json:"artifacts"`
}

func parseManifest(raw json.RawMessage) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if m.Schema != 1 {
		return nil, fmt.Errorf("unsupported manifest schema %d", m.Schema)
	}
	if !gameIDRe.MatchString(m.GameID) {
		return nil, fmt.Errorf("gameId must be lowercase kebab-case, got %q", m.GameID)
	}
	if !versionRe.MatchString(m.Version) {
		return nil, fmt.Errorf("version must be semver X.Y.Z, got %q", m.Version)
	}
	if m.DisplayName == "" {
		return nil, errors.New("displayName is required")
	}
	if m.Players.Min < 1 || m.Players.Max < m.Players.Min {
		return nil, fmt.Errorf("invalid player range %d-%d", m.Players.Min, m.Players.Max)
	}
	if !boards[m.Board] {
		return nil, fmt.Errorf("unknown board kind %q", m.Board)
	}
	return &m, nil
}

// validatePackage runs all three gates and returns the parsed manifest + wasm
// sha on success.
func validatePackage(ctx context.Context, rawManifest json.RawMessage, wasm []byte) (*Manifest, string, error) {
	m, err := parseManifest(rawManifest)
	if err != nil {
		return nil, "", err
	}
	if len(wasm) == 0 {
		return nil, "", errors.New("empty wasm")
	}
	if len(wasm) > maxWasmBytes {
		return nil, "", fmt.Errorf("wasm too large: %d bytes (max %d)", len(wasm), maxWasmBytes)
	}
	sum := sha256.Sum256(wasm)
	sha := hex.EncodeToString(sum[:])
	if m.Artifacts.Wasm != "" && m.Artifacts.Wasm != sha {
		return nil, "", errors.New("manifest wasm hash does not match the uploaded wasm")
	}

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(validateMemPage))
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, "", fmt.Errorf("invalid wasm module: %w", err)
	}

	// Gate 2: imports allow-list.
	for _, fn := range compiled.ImportedFunctions() {
		mod, name, _ := fn.Import()
		if !allowedImportModules[mod] {
			return nil, "", fmt.Errorf("disallowed import %q from module %q (only WASI stdio is permitted)", name, mod)
		}
	}
	if _, ok := compiled.ExportedFunctions()["_start"]; !ok {
		return nil, "", errors.New("wasm does not export _start (not a Bordiko guest)")
	}

	// Gate 3: run a setup scenario end-to-end under limits.
	if err := runSetupScenario(ctx, rt, compiled, m); err != nil {
		return nil, "", fmt.Errorf("setup scenario failed: %w", err)
	}
	return m, sha, nil
}

func runSetupScenario(ctx context.Context, rt wazero.Runtime, compiled wazero.CompiledModule, m *Manifest) error {
	ctx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()

	players := make([]string, 0, m.Players.Min)
	for i := 0; i < m.Players.Min; i++ {
		players = append(players, fmt.Sprintf("p%d", i))
	}
	cmd, _ := json.Marshal(map[string]any{"op": "setup", "players": players, "seed": "validate"})

	var stdout, stderr bytes.Buffer
	cfg := wazero.NewModuleConfig().WithName("").
		WithStdin(bytes.NewReader(cmd)).WithStdout(&stdout).WithStderr(&stderr).
		WithStartFunctions()
	mod, err := rt.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return fmt.Errorf("instantiate: %w", err)
	}
	defer mod.Close(ctx)

	if _, err := mod.ExportedFunction("_start").Call(ctx); err != nil {
		var exitErr *sys.ExitError
		if errors.As(err, &exitErr) {
			if code := exitErr.ExitCode(); code != 0 {
				return fmt.Errorf("guest exited %d: %s", code, stderr.String())
			}
		} else {
			return fmt.Errorf("guest trap: %w", err)
		}
	}

	var state struct {
		G    json.RawMessage `json:"G"`
		Flow json.RawMessage `json:"flow"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &state); err != nil {
		return fmt.Errorf("setup did not return valid state JSON: %w", err)
	}
	if len(state.G) == 0 || len(state.Flow) == 0 {
		return errors.New("setup output missing G/flow")
	}
	return nil
}
