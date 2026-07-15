package main

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type Server struct {
	store           Store
	requireModerate bool
	now             func() time.Time
}

func NewServer(store Store, requireModerate bool, now func() time.Time) *Server {
	return &Server{store: store, requireModerate: requireModerate, now: now}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /readyz", s.health)
	mux.HandleFunc("POST /publish", s.publish)
	mux.HandleFunc("GET /games", s.listGames)
	mux.HandleFunc("GET /games/{id}", s.gameDetail)
	mux.HandleFunc("GET /games/{id}/wasm", s.latestWasm)
	mux.HandleFunc("GET /games/{id}/version", s.latestVersion)
	mux.HandleFunc("GET /games/{id}/assets/{assetId}", s.asset)
	mux.HandleFunc("GET /games/{id}/ui", s.ui)
	mux.HandleFunc("GET /games/{id}/versions/{version}/wasm", s.versionWasm)
	mux.HandleFunc("POST /games/{id}/versions/{version}/moderate", s.moderate)
	mux.HandleFunc("POST /games/{id}/rate", s.rate)
	mux.HandleFunc("GET /games/{id}/rating", s.userRating)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"service": "registry", "status": "ok"})
}

type publishRequest struct {
	Manifest json.RawMessage   `json:"manifest"`
	Wasm     string            `json:"wasm"`             // base64-encoded game.wasm
	Assets   map[string]string `json:"assets,omitempty"` // assetId -> base64 image (Option 1c)
	UI       string            `json:"ui,omitempty"`     // self-contained sandboxed UI bundle html (Option 2)
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	wasm, err := base64.StdEncoding.DecodeString(req.Wasm)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_wasm", "wasm must be base64")
		return
	}

	m, sha, err := validatePackage(r.Context(), req.Manifest, wasm)
	if err != nil {
		// Validation failure is the applicant's problem, not a server error.
		writeErr(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}

	// Gate 4 (Option 1c): the developer's image assets must be real, small rasters.
	assets, err := validateAssets(req.Assets)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "bad_assets", err.Error())
		return
	}
	// Gate 5 (Option 2): the optional sandboxed UI bundle (size-guarded).
	ui, err := validateUI(req.UI)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "bad_ui", err.Error())
		return
	}

	status := "published"
	if s.requireModerate {
		status = "pending"
	}
	v := &GameVersion{
		GameID:      m.GameID,
		Version:     m.Version,
		DisplayName: m.DisplayName,
		Board:       m.Board,
		MinPlayers:  m.Players.Min,
		MaxPlayers:  m.Players.Max,
		WasmSHA:     sha,
		WasmBytes:   len(wasm),
		Status:      status,
		Manifest:    req.Manifest,
		CreatedAt:   s.now(),
	}
	if err := s.store.Publish(v, wasm); err != nil {
		writeErr(w, http.StatusConflict, "publish_failed", err.Error())
		return
	}
	if err := s.store.PutAssets(v.GameID, v.Version, assets); err != nil {
		log.Printf("store assets for %s@%s: %v", v.GameID, v.Version, err)
	}
	if err := s.store.PutUI(v.GameID, v.Version, ui); err != nil {
		log.Printf("store ui for %s@%s: %v", v.GameID, v.Version, err)
	}
	log.Printf("published %s@%s (%s, %d bytes, %d assets, ui=%v)", v.GameID, v.Version, v.Status, v.WasmBytes, len(assets), len(ui) > 0)
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) listGames(w http.ResponseWriter, _ *http.Request) {
	games := s.store.ListLatest()
	ratings := s.store.Ratings()
	for i := range games {
		if agg, ok := ratings[games[i].GameID]; ok {
			games[i].Rating = agg.Avg
			games[i].RatingCount = agg.Count
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"games": games})
}

// rate records a user's star rating for a game. The gateway (which knows the
// authenticated user) is the only intended caller — it injects a trusted userId.
func (s *Server) rate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID string `json:"userId"`
		Stars  int    `json:"stars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" || body.Stars < 1 || body.Stars > 5 {
		writeErr(w, http.StatusBadRequest, "bad_request", "userId and stars (1-5) required")
		return
	}
	gameID := r.PathValue("id")
	if err := s.store.RateGame(gameID, body.UserID, body.Stars); err != nil {
		writeErr(w, http.StatusInternalServerError, "rate_failed", err.Error())
		return
	}
	agg := s.store.Ratings()[gameID]
	writeJSON(w, http.StatusOK, map[string]any{"gameId": gameID, "rating": agg.Avg, "ratingCount": agg.Count})
}

// userRating returns a user's own stars for a game (via ?userId=) plus the
// aggregate — lets the client pre-fill the rater with what the user already gave.
func (s *Server) userRating(w http.ResponseWriter, r *http.Request) {
	gameID := r.PathValue("id")
	stars, _ := s.store.UserRating(gameID, r.URL.Query().Get("userId"))
	agg := s.store.Ratings()[gameID]
	writeJSON(w, http.StatusOK, map[string]any{"gameId": gameID, "stars": stars, "rating": agg.Avg, "ratingCount": agg.Count})
}

func (s *Server) gameDetail(w http.ResponseWriter, r *http.Request) {
	versions := s.store.Versions(r.PathValue("id"))
	if len(versions) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "no such game")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gameId": r.PathValue("id"), "versions": versions})
}

func (s *Server) latestWasm(w http.ResponseWriter, r *http.Request) {
	s.serveWasm(w, r.PathValue("id"), "")
}

// latestVersion reports which build of a game is published right now, without
// shipping the wasm. The game-host asks this when a match is created so it can
// pin that version onto the match (and notice that an update has landed) at the
// cost of a few bytes rather than a megabyte.
func (s *Server) latestVersion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, v := range s.store.ListLatest() {
		if v.GameID == id {
			writeJSON(w, http.StatusOK, map[string]any{"gameId": id, "version": v.Version, "sha": v.WasmSHA})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not_found", "no such published game")
}

func (s *Server) versionWasm(w http.ResponseWriter, r *http.Request) {
	s.serveWasm(w, r.PathValue("id"), r.PathValue("version"))
}

// asset serves a game's uploaded image (Option 1c) from its latest published
// version. The content type is sniffed from the bytes; assets are immutable per
// version, so they cache hard.
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	blob, ok := s.store.LoadAsset(r.PathValue("id"), r.PathValue("assetId"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(blob))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(blob)
}

// ui serves a game's self-contained sandboxed UI bundle (Option 2). The gateway
// wraps this with the sandbox CSP before it reaches the browser iframe.
func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	html, ok := s.store.LoadUI(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no ui bundle for this game")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(html)
}

func (s *Server) serveWasm(w http.ResponseWriter, gameID, version string) {
	blob, meta, ok := s.store.LoadWasm(gameID, version)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such published game version")
		return
	}
	w.Header().Set("Content-Type", "application/wasm")
	w.Header().Set("X-Bordiko-Version", meta.Version)
	w.Header().Set("X-Bordiko-Sha256", meta.WasmSHA)
	_, _ = w.Write(blob)
}

func (s *Server) moderate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"` // "approve" | "reject"
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	status := ""
	switch body.Action {
	case "approve":
		status = "published"
	case "reject":
		status = "rejected"
	default:
		writeErr(w, http.StatusBadRequest, "bad_action", "action must be approve or reject")
		return
	}
	if !s.store.SetStatus(r.PathValue("id"), r.PathValue("version"), status) {
		writeErr(w, http.StatusNotFound, "not_found", "no such game version")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"gameId": r.PathValue("id"), "version": r.PathValue("version"), "status": status})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
