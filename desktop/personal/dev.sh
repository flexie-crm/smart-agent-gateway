#!/bin/bash
# Run the personal edition from this working tree (KB/36).
#
#   ./desktop/personal/dev.sh            edit a page and see it, without a build
#   ./desktop/personal/dev.sh --built    serve the built pages instead, as the app does
#
# This is the stack the application carries, minus the window around it: one
# process, its own MariaDB, both pages, the worker, and no broker. What a
# packaged build adds is an icon, so everything that can go wrong with the STACK
# goes wrong here first, where the loop is seconds rather than minutes.
#
# The point of it is the loop. `desktop/personal/build.sh` builds two front ends, a Go
# binary and a Rust application, and installing the result to move a label is
# most of ten minutes. Here the pages are served by their own build tool, at
# 127.0.0.1 on the gateway's own port, so a saved file is a reload and a
# rebuild of anything is a rebuild of nothing.
#
# One origin, deliberately. The gateway passes /  and /chat/ to the build tools
# rather than the browser talking to them directly, so the cookies, the paths
# and the redirects are the ones the shipped application has. Development on
# its own port is a different deployment, and its bugs are its own.
#
# Everything it makes lives under desktop/.local, which is ignored by git and
# safe to delete: doing so is how you get back to a first run.
set -euo pipefail
# Job control, so each build tool we start becomes its own process group and can
# be taken down with the vite that npm spawned underneath it. Without this they
# all share ours, killing npm leaves vite holding the port, and the next run
# dies on --strictPort.
set -m

cd "$(dirname "$0")/../.."
ROOT=$(pwd)
OUT=$ROOT/desktop/.local
STATE=$OUT/state
PORT=${SAG_DESKTOP_PORT:-8123}
CONSOLE_PORT=${SAG_CONSOLE_DEV_PORT:-5174}
CHAT_PORT=${SAG_CHAT_DEV_PORT:-5173}

BUILT=0
[ "${1:-}" = "--built" ] && BUILT=1

# The database server, taken from whatever MariaDB this machine has. A release
# builds its own (see the bundler's header); for running the thing, the one
# already installed is the same server.
if [ ! -x "$OUT/mariadb/bin/mariadbd" ]; then
	PREFIX=${MARIADB_PREFIX:-}
	if [ -z "$PREFIX" ]; then
		server=$(command -v mariadbd || command -v mysqld || true)
		[ -n "$server" ] || {
			echo "No MariaDB on this machine to bundle." >&2
			echo "Install one (brew install mariadb) or set MARIADB_PREFIX." >&2
			exit 1
		}
		PREFIX=$(dirname "$(dirname "$(readlink -f "$server" 2>/dev/null || echo "$server")")")
	fi
	echo "==> bundling the database from $PREFIX"
	"$ROOT/desktop/personal/mariadb/bundle-macos.sh" "$PREFIX" "$OUT/mariadb"
fi

echo "==> building the gateway"
(cd orchestrator && go build -o "$OUT/sag" ./cmd/sag)

mkdir -p "$STATE"
export SAG_PERSONAL_BUNDLE=$OUT/mariadb
export SAG_PERSONAL_STATE=$STATE
export SAG_HTTP_ADDR=127.0.0.1:$PORT
export SAG_LOG_FORMAT=${SAG_LOG_FORMAT:-console}

# VITE_SAG_PERSONAL is what makes these personal-edition pages rather than
# pages that ask at run time what they are. It is set for the build tools and
# for a --built build alike, because a development loop that runs the OTHER
# edition is a loop that proves nothing about this one.
export VITE_SAG_PERSONAL=1

PAGES=()
stop_pages() {
	for pid in ${PAGES[@]+"${PAGES[@]}"}; do
		kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
	done
}
trap stop_pages EXIT

if [ "$BUILT" = 1 ]; then
	# The chat is built for /chat/, not merely served there: its asset URLs are
	# absolute, so a build made for the root asks for /assets/..., the console's
	# catch-all answers, and the chat is blank with no error anywhere.
	echo "==> building the console"
	(cd admin-ui && SAG_OUT_DIR=dist/personal npm run build >/dev/null)
	echo "==> building the chat for /chat/"
	(cd chat-ui && SAG_OUT_DIR=dist/personal npm run build >/dev/null)
	# The personal edition's own directories, not the ones a server serves
	# (build.sh says why).
	export SAG_CONSOLE_DIR=$ROOT/admin-ui/dist/personal
	export SAG_CHAT_DIR=$ROOT/chat-ui/dist/personal
else
	echo "==> starting the console and the chat"
	(cd admin-ui && npm run dev -- --port "$CONSOLE_PORT" --strictPort >"$OUT/console-dev.log" 2>&1) &
	PAGES+=($!)
	(cd chat-ui && VITE_BASE_PATH=/chat/ npm run dev -- --port "$CHAT_PORT" --strictPort >"$OUT/chat-dev.log" 2>&1) &
	PAGES+=($!)
	export SAG_CONSOLE_DIR=http://127.0.0.1:$CONSOLE_PORT
	export SAG_CHAT_DIR=http://127.0.0.1:$CHAT_PORT
fi

# Wait for each of them, and refuse to go on without one.
#
# The port is pinned rather than picked, because the gateway has to be told
# where to send people before either has started. That means a dev server left
# over from another window owns it, ours exits, and the ports still ANSWER: the
# gateway proxies to somebody else's, built for a different path with a
# different edition compiled in, and the chat comes up blank with nothing in any
# log. So a build tool that did not start is a stop, with its own reason
# printed, rather than a banner promising two addresses that are not ours.
await_page() {
	local name=$1 port=$2 base=$3 pid=$4 log=$5
	local page=""
	for _ in $(seq 1 120); do
		if ! kill -0 "$pid" 2>/dev/null; then
			echo >&2
			echo "The $name did not start:" >&2
			sed 's/^/    /' "$log" >&2
			if lsof -ti "tcp:$port" -sTCP:LISTEN >/dev/null 2>&1; then
				echo "    Something else is already on port $port. Stop it, or set" >&2
				echo "    SAG_CONSOLE_DEV_PORT / SAG_CHAT_DEV_PORT." >&2
			fi
			exit 1
		fi
		page=$(curl -sf "http://127.0.0.1:$port$base" || true)
		if [ -n "$page" ]; then
			# It answered, but is it OURS? A left-over server on this port
			# answers just as readily, from a different working tree with a
			# different path compiled in, and that failure is silent all the way
			# to a blank page. The base path is the cheapest thing that differs.
			if ! grep -q "\"$base@vite/client\"" <<<"$page"; then
				echo >&2
				echo "The $name on port $port is not the one we started: it serves" >&2
				echo "itself from somewhere other than $base. Stop whatever is on" >&2
				echo "that port and run this again." >&2
				exit 1
			fi
			return 0
		fi
		sleep 1
	done
	echo "The $name never answered on port $port." >&2
	exit 1
}

if [ "$BUILT" = 0 ]; then
	await_page "console" "$CONSOLE_PORT" "/" "${PAGES[0]}" "$OUT/console-dev.log"
	await_page "chat" "$CHAT_PORT" "/chat/" "${PAGES[1]}" "$OUT/chat-dev.log"
fi

cat <<BANNER

  Console   http://127.0.0.1:$PORT/
  Chat      http://127.0.0.1:$PORT/chat/

$(if [ "$BUILT" = 1 ]; then
	echo "  Serving the BUILT pages. Rebuild them to see a change."
else
	echo "  The pages reload as you edit them. Their logs are in desktop/.local."
fi)

  A first run creates the database and applies every migration, so give it half
  a minute. There is no sign-in: the owner is seeded and the window is already
  theirs. Ctrl-C stops everything, database last.

BANNER

# Not exec'd: replacing this shell would take the trap with it and leave two
# build tools running with nobody to stop them.
"$OUT/sag" personal
