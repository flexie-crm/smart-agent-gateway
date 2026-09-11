#!/usr/bin/env bash
# Runs the browser end-to-end gate: a fresh scratch database, the real
# orchestrator on the scripted model (cmd/sag-e2e), then Playwright driving the
# real chat UI against it. Separate from `make ci` because it needs a browser
# and a running server. Everything is torn down on exit.
#
# Config via env, with local-dev defaults:
#   SAG_E2E_DB        database name to drop+recreate (default flexie_sag_e2e)
#   SAG_E2E_MYSQL     mysql client args for admin ops (default: current user socket)
#   SAG_E2E_DSN       DSN the server connects with (default: sagtest TCP user)
#   SAG_SESSION_SECRET, SAG_ENCRYPTION_KEYS  reused from the environment
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
DB="${SAG_E2E_DB:-flexie_sag_e2e}"
MYSQL="${SAG_E2E_MYSQL:-mysql}"
PORT="${SAG_E2E_PORT:-8090}"

export SAG_DB_DSN="${SAG_E2E_DSN:-sagtest:sagtestpw@tcp(127.0.0.1:3306)/${DB}?parseTime=true}"
export SAG_HTTP_ADDR=":${PORT}"
export SAG_BASE_URL="http://localhost:${PORT}"
export SAG_ALLOWED_ORIGINS="http://localhost:5273"
export SAG_LOG_LEVEL="${SAG_LOG_LEVEL:-warn}"
export SAG_SESSION_SECRET="${SAG_SESSION_SECRET:-37314ed2e5127d6d2d1375099e2fbd2e5acfd042494dc8f640e0ad4fb3b7336a}"
export SAG_ENCRYPTION_KEYS="${SAG_ENCRYPTION_KEYS:-1:92662f7e16659f6caf017e53c1d0d5259af4f69313f74cda93c7e5e039e6c0a7}"
export SAG_ENCRYPTION_PRIMARY_KEY_ID="${SAG_ENCRYPTION_PRIMARY_KEY_ID:-1}"

echo "e2e: resetting database ${DB}"
$MYSQL -e "DROP DATABASE IF EXISTS \`${DB}\`; CREATE DATABASE \`${DB}\`;"

echo "e2e: building the scripted server"
( cd "$REPO/orchestrator" && go build -o "$REPO/orchestrator/bin/sag-e2e" ./cmd/sag-e2e )

echo "e2e: starting the scripted server on :${PORT}"
"$REPO/orchestrator/bin/sag-e2e" >"$REPO/chat-ui/e2e/server.log" 2>&1 &
SERVER_PID=$!
cleanup() { kill "$SERVER_PID" 2>/dev/null || true; }
trap cleanup EXIT

# Wait for the server to be listening before Playwright starts the UI.
#
# Ninety seconds, not thirty. This gate creates a scratch database and migrates
# it from nothing on every run, and there are 55 migrations now where there were
# a handful when this was written. It began failing as "server did not come up",
# which reads as a broken server and was a boot that had not finished: the
# migrations ran, seeding started, and the poll gave up and killed the process
# mid-insert. The log said so ("seed failed: context canceled") and nothing else
# did.
#
# It also stops early when the process DIES, rather than waiting out the whole
# ceiling for something that is never going to answer.
for _ in $(seq 1 90); do
  if curl -sf -o /dev/null "http://localhost:${PORT}/healthz"; then break; fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "e2e: the server exited before it was ready"
    cat "$REPO/chat-ui/e2e/server.log"
    exit 1
  fi
  sleep 1
done
curl -sf -o /dev/null "http://localhost:${PORT}/healthz" || { echo "e2e: server did not come up"; cat "$REPO/chat-ui/e2e/server.log"; exit 1; }

# A place for a fixture that comes from somewhere else.
#
# The seeded conversations are shaped after real ones, which is most of what a
# fixture needs to be. It is not everything: proving a scroll against a real
# transcript, with its real heights, is a different claim from proving it against
# something built to resemble one. This runs after the seed and before the
# browser, so a script can put a copied conversation into the scratch database.
if [ -n "${SAG_E2E_AFTER_SEED:-}" ]; then
  echo "e2e: running ${SAG_E2E_AFTER_SEED}"
  bash "${SAG_E2E_AFTER_SEED}"
fi

echo "e2e: running Playwright"
cd "$REPO/chat-ui"
npx playwright test "$@"
