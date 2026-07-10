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
	mux.HandleFunc("GET /games/{id}/versions/{version}/wasm", s.versionWasm)
	mux.HandleFunc("POST /games/{id}/versions/{version}/moderate", s.moderate)
	mux.HandleFunc("POST /games/{id}/rate", s.rate)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"service": "registry", "status": "ok"})
}

type publishRequest struct {
	Manifest json.RawMessage `json:"manifest"`
	Wasm     string          `json:"wasm"` // base64-encoded game.wasm
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
	log.Printf("published %s@%s (%s, %d bytes)", v.GameID, v.Version, v.Status, v.WasmBytes)
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

func (s *Server) versionWasm(w http.ResponseWriter, r *http.Request) {
	s.serveWasm(w, r.PathValue("id"), r.PathValue("version"))
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
