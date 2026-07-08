package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

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
