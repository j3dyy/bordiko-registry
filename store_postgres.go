package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the durable Store used when DATABASE_URL is set. Unlike the
// filesystem-backed LocalStore, the wasm bytes live in Postgres (bytea), so a
// published game survives ephemeral redeploys where the container filesystem is
// discarded — the caveat that motivated this store.
type PostgresStore struct {
	ctx  context.Context
	pool *pgxpool.Pool
}

const registrySchemaSQL = `
CREATE TABLE IF NOT EXISTS game_versions (
    game_id      text NOT NULL,
    version      text NOT NULL,
    display_name text NOT NULL,
    board        text NOT NULL,
    min_players  int  NOT NULL,
    max_players  int  NOT NULL,
    wasm_sha     text NOT NULL,
    wasm_bytes   int  NOT NULL,
    status       text NOT NULL,
    manifest     jsonb NOT NULL,
    wasm         bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (game_id, version)
);`

func NewPostgresStore(ctx context.Context, url string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, registrySchemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &PostgresStore{ctx: ctx, pool: pool}, nil
}

func (s *PostgresStore) Publish(v *GameVersion, wasm []byte) error {
	_, err := s.pool.Exec(s.ctx,
		`INSERT INTO game_versions
		   (game_id, version, display_name, board, min_players, max_players,
		    wasm_sha, wasm_bytes, status, manifest, wasm, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		v.GameID, v.Version, v.DisplayName, v.Board, v.MinPlayers, v.MaxPlayers,
		v.WasmSHA, v.WasmBytes, v.Status, []byte(v.Manifest), wasm, v.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return fmt.Errorf("%s@%s already exists", v.GameID, v.Version)
	}
	return err
}

func (s *PostgresStore) ListLatest() []GameVersion {
	rows, err := s.pool.Query(s.ctx,
		`SELECT DISTINCT ON (game_id)
		    game_id, version, display_name, board, min_players, max_players,
		    wasm_sha, wasm_bytes, status, manifest, created_at
		 FROM game_versions
		 WHERE status = 'published'
		 ORDER BY game_id, created_at DESC`)
	if err != nil {
		return []GameVersion{}
	}
	defer rows.Close()
	return scanVersions(rows)
}

func (s *PostgresStore) Versions(gameID string) []GameVersion {
	rows, err := s.pool.Query(s.ctx,
		`SELECT game_id, version, display_name, board, min_players, max_players,
		        wasm_sha, wasm_bytes, status, manifest, created_at
		 FROM game_versions
		 WHERE game_id = $1
		 ORDER BY created_at ASC`, gameID)
	if err != nil {
		return []GameVersion{}
	}
	defer rows.Close()
	return scanVersions(rows)
}

func (s *PostgresStore) LoadWasm(gameID, version string) ([]byte, *GameVersion, bool) {
	var (
		v        GameVersion
		manifest []byte
		wasm     []byte
	)
	// An empty version means "the latest published version".
	query := `SELECT game_id, version, display_name, board, min_players, max_players,
	                 wasm_sha, wasm_bytes, status, manifest, created_at, wasm
	          FROM game_versions WHERE game_id = $1 AND version = $2`
	args := []any{gameID, version}
	if version == "" {
		query = `SELECT game_id, version, display_name, board, min_players, max_players,
		                wasm_sha, wasm_bytes, status, manifest, created_at, wasm
		         FROM game_versions
		         WHERE game_id = $1 AND status = 'published'
		         ORDER BY created_at DESC LIMIT 1`
		args = []any{gameID}
	}
	err := s.pool.QueryRow(s.ctx, query, args...).Scan(
		&v.GameID, &v.Version, &v.DisplayName, &v.Board, &v.MinPlayers, &v.MaxPlayers,
		&v.WasmSHA, &v.WasmBytes, &v.Status, &manifest, &v.CreatedAt, &wasm)
	if err != nil {
		return nil, nil, false
	}
	v.Manifest = manifest
	return wasm, &v, true
}

func (s *PostgresStore) SetStatus(gameID, version, status string) bool {
	ct, err := s.pool.Exec(s.ctx,
		`UPDATE game_versions SET status = $3 WHERE game_id = $1 AND version = $2`,
		gameID, version, status)
	return err == nil && ct.RowsAffected() > 0
}

func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

func scanVersions(rows pgx.Rows) []GameVersion {
	out := []GameVersion{}
	for rows.Next() {
		var (
			v        GameVersion
			manifest []byte
		)
		if err := rows.Scan(
			&v.GameID, &v.Version, &v.DisplayName, &v.Board, &v.MinPlayers, &v.MaxPlayers,
			&v.WasmSHA, &v.WasmBytes, &v.Status, &manifest, &v.CreatedAt); err != nil {
			continue
		}
		v.Manifest = manifest
		out = append(out, v)
	}
	return out
}
