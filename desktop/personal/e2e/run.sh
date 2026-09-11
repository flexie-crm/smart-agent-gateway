#!/usr/bin/env bash
# The desktop end-to-end gate.
#
#   make desktop-e2e
#
# What it drives is the BUILT payload in desktop mode: the real gateway, the real
# bundled MariaDB, the real console and chat, on a scratch state directory and a
# free port. Nothing here is mocked, because the failures it exists to catch were
# never in the parts a mock replaces.
#
# Why this is separate from `make e2e`. That one drives the chat against a server
# deployment on a shared database. This one drives an INSTALLATION: the first
# run, the seeding, the two applications sharing a gateway, the setup that has to
# be walked before anything can answer. They are different products from the
# outside even though they are one codebase, and every regression this session
# was in the difference.
#
# It uses the same Playwright as the chat's gate (chat-ui/node_modules), rather
# than a second browser install.
set -euo pipefail
# Without pipefail a `make desktop-e2e | tail` reports tail's exit code, so a
# failing gate reads as a passing one. That happened, and an application was
# installed off the back of it.
set -o pipefail

# THREE levels up, not two. This script is at desktop/personal/e2e/run.sh, and it
# used to be at desktop/e2e/run.sh: the reorganisation into personal/ and
# enterprise/ moved it one level deeper and this was left behind, so REPO pointed
# at desktop/ and every path built from it gained an extra segment. The gate
# looked for the payload at desktop/desktop/.local/payload and refused to run.
#
# It failed the same way every time it was run, which was not once between the
# move and now, so nothing said.
REPO="$(cd "$(dirname "$0")/../../.." && pwd)"
PAYLOAD="${SAG_E2E_PAYLOAD:-$REPO/desktop/.local/payload}"
STATE="$(mktemp -d "${TMPDIR:-/tmp}/sag-e2e.XXXXXX")"
PORT="${SAG_DESKTOP_E2E_PORT:-8199}"
LOG="$REPO/desktop/personal/e2e/gateway.log"

if [ ! -x "$PAYLOAD/sag" ]; then
	echo "desktop-e2e: no built payload at $PAYLOAD" >&2
	echo "             run ./desktop/personal/build.sh --no-engine first" >&2
	exit 1
fi

cleanup() {
	# The gateway owns a database; ask it to stop rather than killing it, so the
	# scratch directory is not left with a server still writing into it.
	if [ -n "${SERVER_PID:-}" ]; then
		kill -TERM "$SERVER_PID" 2>/dev/null || true
		for _ in $(seq 1 30); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 1; done
		kill -KILL "$SERVER_PID" 2>/dev/null || true
	fi
	rm -rf "$STATE"
}
trap cleanup EXIT

# The specs live here and the toolchain lives in chat-ui, so node has nothing to
# walk up to. Linked rather than installed a second time: a second copy of
# Playwright to satisfy a resolution rule would be a poor trade, and the link is
# ignored by git and remade whenever it is missing.
if [ ! -e "$REPO/desktop/personal/e2e/node_modules" ]; then
	# -f and -n, because what is usually there is a BROKEN link rather than
	# nothing: this one pointed two levels up from when the script lived at
	# desktop/e2e, and `[ -e ]` is false for a broken link, so the guard let us in
	# and then `ln -s` refused because the path exists. Replacing is always right
	# here; the link is ours and points at one place.
	ln -sfn ../../../chat-ui/node_modules "$REPO/desktop/personal/e2e/node_modules"
fi

echo "desktop-e2e: starting a first run in $STATE"
SAG_HTTP_ADDR="127.0.0.1:$PORT" \
SAG_PERSONAL_BUNDLE="$PAYLOAD/mariadb" \
SAG_PERSONAL_STATE="$STATE" \
SAG_CONSOLE_DIR="$PAYLOAD/console" \
SAG_CHAT_DIR="$PAYLOAD/chat" \
SAG_LOG_FORMAT=json \
	"$PAYLOAD/sag" personal >"$LOG" 2>&1 &
SERVER_PID=$!

# A first run creates a database and applies every migration, so this is a
# generous wait rather than an optimistic one.
for _ in $(seq 1 180); do
	if curl -sf -o /dev/null "http://127.0.0.1:$PORT/healthz"; then break; fi
	if ! kill -0 "$SERVER_PID" 2>/dev/null; then
		echo "desktop-e2e: the gateway exited during startup" >&2
		tail -20 "$LOG" >&2
		exit 1
	fi
	sleep 1
done
curl -sf -o /dev/null "http://127.0.0.1:$PORT/healthz" || {
	echo "desktop-e2e: the gateway never answered" >&2
	tail -20 "$LOG" >&2
	exit 1
}

echo "desktop-e2e: driving both surfaces"
cd "$REPO/chat-ui"
SAG_DESKTOP_E2E_URL="http://127.0.0.1:$PORT" \
	npx playwright test --config "$REPO/chat-ui/desktop-e2e.config.ts" "$@"
