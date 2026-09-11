# Flexie SAG — Orchestrator

The Go backend: REST API, SSE/WebSocket streaming, agent runtime, model
gateway, tool runtime, and background workers — **one codebase, one binary,
two starting angles**:

```
sag server      # web mode: HTTP API, SSE chat streaming, WS hub, MCP surface
sag worker      # jobs mode: queue consumers, scheduler, model runtime supervisor
sag migrate     # schema migrations (up|down|status|version), embedded in the binary
sag bootstrap   # create a workspace and its first user in an empty database
```

Both modes share every internal package; they differ only in what they wire
at boot (see `KB/05` in the repo root).

## Layout

```
cmd/sag/                 entry point + subcommand dispatch
internal/config/         env-based configuration (shared by all modes)
internal/app/            shared application core (business logic layer)
internal/api/            HTTP layer (server mode only)
internal/worker/         jobs-mode wiring (queue consumers, scheduler)
internal/provider/       model gateway contract (vendor adapters implement it)
internal/tool/           the single tool contract + registry (all surfaces)
internal/queue/          job distribution contract (NATS JetStream signals,
                         durable MariaDB job rows)
internal/modelruntime/   local model lifecycle supervisor (Ollama, vLLM)
internal/store/          persistence interface (MariaDB impl in sqlstore/)
internal/migrations/     goose migrations, embedded via go:embed
```

## Develop

```bash
cp .env.example .env     # fill in real values (gitignored)
set -a; source .env; set +a

go build ./... && go vet ./...
go run ./cmd/sag migrate up
go run ./cmd/sag bootstrap -workspace acme -email admin@acme.test -password 'dev-Passw0rd!'
go run ./cmd/sag server
go run ./cmd/sag worker
```

## Test

Everything is tested (see the repo `CLAUDE.md`). The store and API suites run
against a real database; point `SAG_TEST_DSN` at a scratch one and each test
package creates its own isolated copy:

```bash
export SAG_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/flexie_sag_test?parseTime=true'
go test ./...
```

Without `SAG_TEST_DSN` those suites skip and only the pure-unit tests run.

## Docker

```bash
docker compose up --build
```

Brings up: orchestrator (`sag server`, :8080), worker (`sag worker`),
MariaDB 11.4, NATS 2.10 (JetStream), Redis 7. A GPU/model-host worker is the
same image with `SAG_WORKER_CAPABILITIES=default,gpu,model-host`.
