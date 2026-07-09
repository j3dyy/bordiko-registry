package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// GameVersion is one published build of a game.
type GameVersion struct {
	GameID      string          `json:"gameId"`
	Version     string          `json:"version"`
	DisplayName string          `json:"displayName"`
	Board       string          `json:"board"`
	MinPlayers  int             `json:"minPlayers"`
	MaxPlayers  int             `json:"maxPlayers"`
	WasmSHA     string          `json:"wasmSha"`
	WasmBytes   int             `json:"wasmBytes"`
	Status      string          `json:"status"` // published | pending | rejected
	Manifest    json.RawMessage `json:"manifest"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// Store persists published game versions and their wasm blobs. Two
// implementations exist: LocalStore (in-memory + optional filesystem, for
// dev/single-node) and PostgresStore (durable across ephemeral redeploys, used
// when DATABASE_URL is set). The catalog server depends only on this interface.
type Store interface {
	Publish(v *GameVersion, wasm []byte) error
	ListLatest() []GameVersion
	Versions(gameID string) []GameVersion
	LoadWasm(gameID, version string) ([]byte, *GameVersion, bool)
	SetStatus(gameID, version, status string) bool
	Close() error
}

// LocalStore keeps published games in memory and, if a data dir is configured,
// persists each publish to disk (and reloads on startup) so the catalog
// survives restarts. The wasm blobs live beside the metadata.
type LocalStore struct {
	mu    sync.RWMutex
	games map[string][]*GameVersion // gameID -> versions in publish order
	blobs map[string][]byte         // "gameID@version" -> wasm
	dir   string
	now   func() time.Time
}

func blobKey(gameID, version string) string { return gameID + "@" + version }

func NewLocalStore(dir string, now func() time.Time) (*LocalStore, error) {
	s := &LocalStore{games: map[string][]*GameVersion{}, blobs: map[string][]byte{}, dir: dir, now: now}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := s.loadFromDisk(); err != nil {
			return nil, fmt.Errorf("load registry data: %w", err)
		}
	}
	return s, nil
}

// Publish records a validated game version (and its wasm), optionally to disk.
func (s *LocalStore) Publish(v *GameVersion, wasm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.games[v.GameID] {
		if existing.Version == v.Version {
			return fmt.Errorf("%s@%s already exists", v.GameID, v.Version)
		}
	}
	s.games[v.GameID] = append(s.games[v.GameID], v)
	s.blobs[blobKey(v.GameID, v.Version)] = wasm
	if s.dir != "" {
		return s.persist(v, wasm)
	}
	return nil
}

// ListLatest returns the latest PUBLISHED version of each game.
func (s *LocalStore) ListLatest() []GameVersion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []GameVersion{}
	for _, versions := range s.games {
		var latest *GameVersion
		for _, v := range versions {
			if v.Status == "published" && (latest == nil || v.CreatedAt.After(latest.CreatedAt)) {
				latest = v
			}
		}
		if latest != nil {
			out = append(out, *latest)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GameID < out[j].GameID })
	return out
}

func (s *LocalStore) Versions(gameID string) []GameVersion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []GameVersion{}
	for _, v := range s.games[gameID] {
		out = append(out, *v)
	}
	return out
}

// LoadWasm returns the wasm for a game version. An empty version means "the
// latest published version".
func (s *LocalStore) LoadWasm(gameID, version string) ([]byte, *GameVersion, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if version == "" {
		var latest *GameVersion
		for _, v := range s.games[gameID] {
			if v.Status == "published" && (latest == nil || v.CreatedAt.After(latest.CreatedAt)) {
				latest = v
			}
		}
		if latest == nil {
			return nil, nil, false
		}
		version = latest.Version
	}
	blob, ok := s.blobs[blobKey(gameID, version)]
	if !ok {
		return nil, nil, false
	}
	var meta *GameVersion
	for _, v := range s.games[gameID] {
		if v.Version == version {
			meta = v
		}
	}
	return blob, meta, true
}

// SetStatus updates a version's moderation status (published/pending/rejected).
func (s *LocalStore) SetStatus(gameID, version, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.games[gameID] {
		if v.Version == version {
			v.Status = status
			if s.dir != "" {
				blob := s.blobs[blobKey(gameID, version)]
				_ = s.persist(v, blob)
			}
			return true
		}
	}
	return false
}

func (s *LocalStore) Close() error { return nil }

/* ------------------------------ persistence ------------------------------- */

func (s *LocalStore) persist(v *GameVersion, wasm []byte) error {
	dir := filepath.Join(s.dir, v.GameID, v.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "game.wasm"), wasm, 0o644); err != nil {
		return err
	}
	meta, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644)
}

func (s *LocalStore) loadFromDisk() error {
	gameDirs, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, gd := range gameDirs {
		if !gd.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(s.dir, gd.Name()))
		if err != nil {
			continue
		}
		for _, vd := range versions {
			if !vd.IsDir() {
				continue
			}
			base := filepath.Join(s.dir, gd.Name(), vd.Name())
			metaBytes, err := os.ReadFile(filepath.Join(base, "meta.json"))
			if err != nil {
				continue
			}
			wasm, err := os.ReadFile(filepath.Join(base, "game.wasm"))
			if err != nil {
				continue
			}
			var v GameVersion
			if err := json.Unmarshal(metaBytes, &v); err != nil {
				continue
			}
			s.games[v.GameID] = append(s.games[v.GameID], &v)
			s.blobs[blobKey(v.GameID, v.Version)] = wasm
		}
	}
	return nil
}
