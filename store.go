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
	// Transient rating aggregates — not persisted on the version row; the server
	// fills these from the ratings store when it lists the catalog.
	Rating      float64 `json:"rating"`
	RatingCount int     `json:"ratingCount"`
}

// RatingAgg is a game's aggregated player rating (mean stars + how many raters).
type RatingAgg struct {
	Avg   float64 `json:"avg"`
	Count int     `json:"count"`
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
	// PutAssets stores a version's image assets (assetId -> raw bytes) — the
	// developer's own art for the declarative board (Option 1c).
	PutAssets(gameID, version string, assets map[string][]byte) error
	// LoadAsset returns an asset from a game's LATEST published version.
	LoadAsset(gameID, assetID string) ([]byte, bool)
	// PutUI stores a version's self-contained sandboxed UI bundle (Option 2).
	PutUI(gameID, version string, html []byte) error
	// LoadUI returns a game's UI bundle from its LATEST published version.
	LoadUI(gameID string) ([]byte, bool)
	// RateGame records one user's 1–5 star rating for a game (one per user;
	// re-rating overwrites). Ratings returns the mean+count per game id.
	RateGame(gameID, userID string, stars int) error
	Ratings() map[string]RatingAgg
	// UserRating returns a user's own stars for a game (0/false if unrated), so
	// the client can pre-fill the rater with what they already gave.
	UserRating(gameID, userID string) (int, bool)
	Close() error
}

// LocalStore keeps published games in memory and, if a data dir is configured,
// persists each publish to disk (and reloads on startup) so the catalog
// survives restarts. The wasm blobs live beside the metadata.
type LocalStore struct {
	mu      sync.RWMutex
	games   map[string][]*GameVersion    // gameID -> versions in publish order
	blobs   map[string][]byte            // "gameID@version" -> wasm
	assets  map[string]map[string][]byte // "gameID@version" -> assetId -> bytes
	ui      map[string][]byte            // "gameID@version" -> sandboxed UI bundle html
	ratings map[string]map[string]int    // gameID -> userID -> stars (1..5)
	dir     string
	now     func() time.Time
}

func blobKey(gameID, version string) string { return gameID + "@" + version }

func NewLocalStore(dir string, now func() time.Time) (*LocalStore, error) {
	s := &LocalStore{games: map[string][]*GameVersion{}, blobs: map[string][]byte{}, assets: map[string]map[string][]byte{}, ui: map[string][]byte{}, ratings: map[string]map[string]int{}, dir: dir, now: now}
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

// RateGame records/overwrites a user's star rating (in memory; the durable
// PostgresStore is used in production).
func (s *LocalStore) RateGame(gameID, userID string, stars int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ratings[gameID] == nil {
		s.ratings[gameID] = map[string]int{}
	}
	s.ratings[gameID][userID] = stars
	return nil
}

func (s *LocalStore) Ratings() map[string]RatingAgg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]RatingAgg{}
	for gid, byUser := range s.ratings {
		if len(byUser) == 0 {
			continue
		}
		sum := 0
		for _, st := range byUser {
			sum += st
		}
		out[gid] = RatingAgg{Avg: float64(sum) / float64(len(byUser)), Count: len(byUser)}
	}
	return out
}

func (s *LocalStore) UserRating(gameID, userID string) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if byUser := s.ratings[gameID]; byUser != nil {
		v, ok := byUser[userID]
		return v, ok
	}
	return 0, false
}

func (s *LocalStore) PutAssets(gameID, version string, assets map[string][]byte) error {
	if len(assets) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := blobKey(gameID, version)
	cp := make(map[string][]byte, len(assets))
	for id, b := range assets {
		cp[id] = b
	}
	s.assets[key] = cp
	if s.dir != "" {
		dir := filepath.Join(s.dir, gameID, version, "assets")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for id, b := range cp {
			if err := os.WriteFile(filepath.Join(dir, id), b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *LocalStore) LoadAsset(gameID, assetID string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *GameVersion
	for _, v := range s.games[gameID] {
		if v.Status == "published" && (latest == nil || v.CreatedAt.After(latest.CreatedAt)) {
			latest = v
		}
	}
	if latest == nil {
		return nil, false
	}
	if a := s.assets[blobKey(gameID, latest.Version)]; a != nil {
		b, ok := a[assetID]
		return b, ok
	}
	return nil, false
}

func (s *LocalStore) PutUI(gameID, version string, html []byte) error {
	if len(html) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ui[blobKey(gameID, version)] = html
	if s.dir != "" {
		dir := filepath.Join(s.dir, gameID, version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "ui.html"), html, 0o644)
	}
	return nil
}

func (s *LocalStore) LoadUI(gameID string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *GameVersion
	for _, v := range s.games[gameID] {
		if v.Status == "published" && (latest == nil || v.CreatedAt.After(latest.CreatedAt)) {
			latest = v
		}
	}
	if latest == nil {
		return nil, false
	}
	b, ok := s.ui[blobKey(gameID, latest.Version)]
	return b, ok
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
			// Optional per-version image assets.
			if entries, err := os.ReadDir(filepath.Join(base, "assets")); err == nil {
				m := map[string][]byte{}
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					if b, err := os.ReadFile(filepath.Join(base, "assets", e.Name())); err == nil {
						m[e.Name()] = b
					}
				}
				if len(m) > 0 {
					s.assets[blobKey(v.GameID, v.Version)] = m
				}
			}
			// Optional sandboxed UI bundle.
			if html, err := os.ReadFile(filepath.Join(base, "ui.html")); err == nil {
				s.ui[blobKey(v.GameID, v.Version)] = html
			}
		}
	}
	return nil
}
