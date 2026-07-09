# registry

The marketplace. Developers publish game packages here; the registry validates
them and serves the approved wasm to the game-host on demand — so a new game
becomes playable **without redeploying** anything.

## Why every upload is validated

An open marketplace runs untrusted code, so nothing is trusted on upload. Three
gates run before a package can be played (`validate.go`):

1. **Manifest** — well-formed, kebab-case `gameId`, semver `version`, sane player
   range, known board kind, and (if declared) a `wasm` sha that matches the bytes.
2. **Imports allow-list** — the wasm may import **only** the WASI stdio surface
   (`wasi_snapshot_preview1`). A module importing anything else (network, fs, a
   custom host function) is rejected. This is the same closed surface the runtime
   sandbox already depends on.
3. **Setup scenario** — the wasm is instantiated in wazero under a memory cap +
   timeout and must run `setup` to completion and return valid game state. A
   module that crashes, hangs, or isn't a real Bordiko guest never publishes.

## Config (env)

| Var | Default | Purpose |
| --- | --- | --- |
| `REGISTRY_ADDR` | `:8082` | Listen address |
| `DATABASE_URL` | _(unset)_ | Postgres DSN — stores wasm in `bytea` (**durable**, survives redeploys). Preferred in production. |
| `REGISTRY_DATA_DIR` | _(unset)_ | Filesystem fallback used only when `DATABASE_URL` is unset; lost on ephemeral redeploys |
| `REGISTRY_REQUIRE_MODERATION` | _(unset)_ | If `true`, uploads land as `pending` until approved |

Store selection: `DATABASE_URL` (Postgres bytea) → else `REGISTRY_DATA_DIR`
(filesystem) → else in-memory.

## HTTP API

| Method & path | Purpose |
| --- | --- |
| `POST /publish` | `{manifest, wasm(base64)}` → validate + publish (or `422` with the reason) |
| `GET /games` | Catalog: latest published version of each game |
| `GET /games/{id}` | All versions of a game |
| `GET /games/{id}/wasm` | Latest published wasm (this is what the game-host fetches) |
| `GET /games/{id}/versions/{v}/wasm` | A specific version's wasm |
| `POST /games/{id}/versions/{v}/moderate` | `{action:"approve"\|"reject"}` |

## Publish a game

```bash
npm run wasm:build                          # build dist/<id>.wasm
npm run registry:run                        # start the registry (:8082)
REGISTRY=http://localhost:8082 \
  node tools/publish.mjs games/eights       # validate + publish
curl -s localhost:8082/games                # see it in the catalog
```

With the game-host started with `REGISTRY_URL=http://localhost:8082`, creating a
match for a game it doesn't have locally pulls the wasm from here, compiles it,
and plays — no redeploy.

## Tests

`go test github.com/bordiko/registry` — the validator accepts a real game wasm,
rejects a hand-crafted module with a forbidden import, and rejects a malformed
manifest.
