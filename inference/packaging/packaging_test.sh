#!/bin/sh
# The gate for how a machine is installed and supervised.
#
# This existed as nothing at all, and two bugs got through that a person only
# found by reading the files:
#
#   1. StartLimitIntervalSec sat in [Service], where systemd IGNORES it, so the
#      restart limit could never trip and a node with a bad token would have
#      restarted forever.
#   2. `systemctl enable --now` does nothing to a service that is already
#      running, so running the installer again to UPGRADE wrote a new binary,
#      left the old process serving, and reported "Installed and running."
#
# Both are asserted below. Neither is the kind of thing a person catches twice.
#
# It runs the REAL installer, as root, in a container, against a stubbed
# `systemctl`: the file writes, the argument parsing and the refusals are all the
# genuine article, and only the service manager is a fake that records what it
# was told to do. A test that stubbed the installer instead would be testing the
# stub.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
IMAGE="debian:bookworm-slim"
FAILED=0

say() { printf '%s\n' "$*"; }
ok() { say "  ok   $*"; }
bad() { say "  FAIL $*"; FAILED=1; }

check() {
	# check <description> <haystack file> <pattern>
	if grep -qE "$3" "$2"; then ok "$1"; else bad "$1 (no /$3/ in $2)"; fi
}
absent() {
	if grep -qE "$3" "$2"; then bad "$1 (found /$3/ in $2)"; else ok "$1"; fi
}

# --- the unit, checked by systemd itself ------------------------------------
#
# Not by us reading it. systemd is the only thing that knows which directives it
# honours and which section they belong in, and it says so when asked.

say "the unit file"
UNIT_OUT=$(mktemp)
if command -v systemd-analyze >/dev/null 2>&1; then
	systemd-analyze verify "$HERE/sag-inference.service" > "$UNIT_OUT" 2>&1 || true
else
	docker run --rm -v "$HERE:/pkg:ro" "$IMAGE" sh -c \
		'apt-get update >/dev/null 2>&1 && apt-get install -y --no-install-recommends systemd >/dev/null 2>&1
		 systemd-analyze verify /pkg/sag-inference.service 2>&1 || true' > "$UNIT_OUT" 2>&1
fi

# The one that bit. An ignored key is a directive that is not in force, and
# systemd reports it and carries on, so nothing else would ever have said.
absent "systemd honours every directive in it" "$UNIT_OUT" "Unknown key"
absent "no unit section is misspelt" "$UNIT_OUT" "Unknown section"

# The rules themselves, so that deleting one is a test failure and not a quiet
# change of behaviour.
check "it restarts on failure" "$HERE/sag-inference.service" "^Restart=always"
# In WHICH section, which is the whole of the bug: the directive was present
# and in the wrong place, so every grep for its name passed. awk tracks the
# section; grep is line based and structurally cannot.
if awk '/^\[/ { section = $0 } /^StartLimitIntervalSec=/ { print section }' \
	"$HERE/sag-inference.service" | grep -qx '\[Unit\]'; then
	ok "the restart limit is in [Unit], where systemd reads it"
else
	bad "the restart limit is not in [Unit], so systemd ignores it"
fi
if awk '/^\[/ { section = $0 } /^StartLimitBurst=/ { print section }' \
	"$HERE/sag-inference.service" | grep -qx '\[Unit\]'; then
	ok "the restart burst is in [Unit] too"
else
	bad "the restart burst is not in [Unit], so systemd ignores it"
fi
check "it escalates rather than looping" "$HERE/sag-inference.service" "^StartLimitBurst="
check "it is asked to stop before it is killed" "$HERE/sag-inference.service" "^KillSignal=SIGTERM"
check "in-flight answers get time to finish" "$HERE/sag-inference.service" "^TimeoutStopSec="
check "it starts on boot" "$HERE/sag-inference.service" "^WantedBy=multi-user.target"
check "it waits for the network it registers over" "$HERE/sag-inference.service" \
	"^After=.*network-online"
check "it may reach the graphics devices" "$HERE/sag-inference.service" "^DeviceAllow=/dev/nvidia"
check "it cannot write outside its own directory" "$HERE/sag-inference.service" \
	"^ProtectSystem=strict"

# --- the installer, run for real against a fake service manager -------------

say ""
say "the installer"
LOG=$(mktemp)
docker run --rm -v "$HERE:/pkg:ro" "$IMAGE" sh -euc '
	# openssl, because the installer uses it to make a machine its own
	# certificate, and a base image without it made the whole script exit under
	# `set -e` at the first case that needed one, taking every later case with
	# it. The failure looked like the cases after it had simply not been written.
	apt-get update >/dev/null 2>&1
	apt-get install -y --no-install-recommends openssl >/dev/null 2>&1
	mkdir -p /stub /work
	# This container is a machine WITH systemd, and it has to say so the way a
	# real one does. The installer no longer asks whether the systemctl binary
	# exists, because plenty of images ship it and are not booted with systemd;
	# it asks whether systemd is running, which is this directory. Without it
	# every case below quietly took the no-init path and the assertions about
	# units failed, which is the fixture being wrong rather than the installer.
	mkdir -p /run/systemd/system
	# ONLY the service manager is a fake, and only because a container has no
	# init to talk to. `useradd`, `install`, `tar` and `chown` are the real ones:
	# stubbing useradd made every later `install -g sag-inference` fail with
	# "invalid user", which is the fixture lying rather than the installer being
	# wrong, and a fixture that lies is a test that proves nothing.
	#
	# It exits 0 for everything, which makes `is-active` succeed: this is a
	# machine ALREADY RUNNING the service, which is the upgrade case that matters.
	printf "#!/bin/sh\necho \"systemctl \$*\" >> /work/calls\nexit 0\n" > /stub/systemctl
	chmod +x /stub/systemctl
	# The binary comes from a local tarball, so nothing here reaches the network.
	mkdir -p /work/payload
	printf "#!/bin/sh\necho fake node\n" > /work/payload/sag-inference
	chmod +x /work/payload/sag-inference
	cp /pkg/sag-inference.service /work/payload/
	tar czf /work/node.tar.gz -C /work/payload sag-inference sag-inference.service
	export PATH=/stub:$PATH

	echo "== refusals =="
	sh /pkg/install.sh --token t 2>&1 | grep -q "url is required" \
		&& echo "REFUSED-NO-URL" || echo "ACCEPTED-NO-URL"
	sh /pkg/install.sh --url https://x 2>&1 | grep -q "token is required" \
		&& echo "REFUSED-NO-TOKEN" || echo "ACCEPTED-NO-TOKEN"
	sh /pkg/install.sh --url https://x --token t --port nope 2>&1 | grep -q "must be a number" \
		&& echo "REFUSED-BAD-PORT" || echo "ACCEPTED-BAD-PORT"
	sh /pkg/install.sh --url https://x --token t --port 70000 2>&1 | grep -q "between 1 and 65535" \
		&& echo "REFUSED-HUGE-PORT" || echo "ACCEPTED-HUGE-PORT"
	sh /pkg/install.sh --url ftp://x --token t 2>&1 | grep -q "https" \
		&& echo "REFUSED-BAD-SCHEME" || echo "ACCEPTED-BAD-SCHEME"

	echo "== a real install =="
	: > /work/calls
	sh /pkg/install.sh --url https://sag.example.com --token abc_secret \
		--from /work/node.tar.gz --accel cpu >/dev/null 2>&1 || echo "INSTALL-FAILED"
	echo "--- what it told the service manager ---"
	cat /work/calls
	echo "--- the settings it wrote ---"
	cat /etc/sag-inference/node.env

	echo "== running it again, which is how a machine is upgraded =="
	: > /work/calls
	echo "SAG_MARKER=kept-from-before" >> /etc/sag-inference/node.env
	sh /pkg/install.sh --url https://sag.example.com --token another_secret \
		--from /work/node.tar.gz --accel cpu >/dev/null 2>&1 || echo "UPGRADE-FAILED"
	echo "--- what it told the service manager ---"
	cat /work/calls
	echo "--- settings after an upgrade ---"
	cat /etc/sag-inference/node.env

	echo "== which build a card asks for =="
	# The whole CUDA path had never been exercised: every case above passes
	# --accel cpu --from, so the capability detection and the download were
	# untested, which is how the fallback to a tarball no build produces
	# survived. Two stubs make the machine a GPU box: one card, one server.
	cat > /stub/nvidia-smi <<"XEOF"
#!/bin/sh
case "$*" in
  *name*) cat /work/card ;;
  *compute_cap*) cat /work/cap ;;
esac
XEOF
	chmod +x /stub/nvidia-smi
	# A release directory holding exactly what release.sh publishes, builds.txt
	# included, so the refusal has something real to read.
	mkdir -p /work/releases
	cp /work/node.tar.gz /work/releases/sag-inference-linux-x86_64-cuda86.tar.gz
	printf "cpu\ncuda86\ncuda103\n" > /work/releases/builds.txt
	cat > /stub/curl <<"XEOF"
#!/bin/sh
# curl -fsSL <url> [-o <file>], serving from the release directory and failing
# the way the real one does: 3 for a URL with no host, 22 for a missing file.
# The first matters as much as the second. A machine added by certificate has no
# gateway address, and the release URL was built from it, so the download became
# "/inference/download/v1/..." and curl refused it with 3 -- which was then
# reported as "there is no build published for your card", about an H200.
url=$2
out=""
[ "$3" = "-o" ] && out=$4
case "$url" in
  http://*|https://*) ;;
  *) echo "curl: (3) URL rejected: No host part in the URL" >&2; exit 3 ;;
esac
f=/work/releases/$(basename "$url")
[ -f "$f" ] || exit 22
if [ -n "$out" ]; then cp "$f" "$out"; else cat "$f"; fi
XEOF
	chmod +x /stub/curl

	echo "NVIDIA A10G" > /work/card; echo "8.6" > /work/cap
	sh /pkg/install.sh --url https://sag.example.com --token t >/dev/null 2>&1 \
		&& echo "CUDA86-INSTALLED" || echo "CUDA86-FAILED"

	# Below the floor the engine needs. A permanent no, and the owner needs to know
	# what to rent instead. This is also the card that used to ask for
	# sag-inference-linux-x86_64-cuda.tar.gz and get a bare 404.
	echo "Tesla T4" > /work/card; echo "7.5" > /work/cap
	sh /pkg/install.sh --url https://sag.example.com --token t > /work/t4.out 2>&1 || true
	echo "--- what a T4 is told ---"
	cat /work/t4.out

	echo "== a machine with no init system =="
	# The common case for a rented GPU: a container with no PID 1 to hand a
	# service to. This used to run to the very end and then say "System has not
	# been booted with systemd as init system, cannot operate, having already
	# written the binary, the certificates and the settings.
	rm -rf /run/systemd/system
	echo "NVIDIA A10G" > /work/card; echo "8.6" > /work/cap
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
		-keyout /work/noinit.key -out /work/noinit.pem -days 30 -subj "/CN=SAG machine authority" >/dev/null 2>&1
	sh /pkg/install.sh --gateway-cert "$(base64 -w0 /work/noinit.pem)" \
		--from /work/node.tar.gz --accel cpu > /work/noinit.out 2>&1 || true
	echo "--- what a machine with no init is told ---"
	cat /work/noinit.out
	echo "--- was a start command left behind? ---"
	[ -x /usr/local/bin/sag-inference-start ] && echo "START-SCRIPT-PRESENT" || echo "START-SCRIPT-MISSING"
	mkdir -p /run/systemd/system

	# A machine added by certificate, which names no gateway. The release URL
	# has to come from somewhere else entirely, and when it did not this asked
	# for a URL with no host and blamed the card for the failure.
	echo "NVIDIA A10G" > /work/card; echo "8.6" > /work/cap
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
		-keyout /work/gw.key -out /work/gw.pem -days 30 -subj "/CN=SAG machine authority" >/dev/null 2>&1
	SAG_RELEASES=http://releases.test/v1 sh /pkg/install.sh \
		--gateway-cert "$(base64 -w0 /work/gw.pem)" > /work/bycert.out 2>&1 || true
	echo "--- installing with a certificate and no gateway ---"
	cat /work/bycert.out

	# Above the floor, nothing published. A temporary no, and a completely
	# different answer: the card is fine and we are the ones behind. Telling
	# this person to go and buy an A100 would be absurd.
	echo "NVIDIA Nextgen 900" > /work/card; echo "13.0" > /work/cap
	sh /pkg/install.sh --url https://sag.example.com --token t > /work/new.out 2>&1 || true
	echo "--- what an unpublished newer card is told ---"
	cat /work/new.out
' > "$LOG" 2>&1 || true

check "it refuses without an address"              "$LOG" "REFUSED-NO-URL"
check "it refuses without a token"                 "$LOG" "REFUSED-NO-TOKEN"
check "it refuses a port that is not a number"     "$LOG" "REFUSED-BAD-PORT"
check "it refuses a port out of range"             "$LOG" "REFUSED-HUGE-PORT"
check "it refuses an address that is not a web one" "$LOG" "REFUSED-BAD-SCHEME"
absent "a good install does not fail"              "$LOG" "INSTALL-FAILED"
absent "an upgrade does not fail"                  "$LOG" "UPGRADE-FAILED"

check "it enables the service so it comes back on boot" "$LOG" "systemctl enable sag-inference"
check "it reloads the unit it just wrote"               "$LOG" "systemctl daemon-reload"

# THE ONE. `enable --now` does nothing to a running service, so an upgrade wrote
# a new binary and left the old process serving while reporting success.
check "it RESTARTS, so a second run actually upgrades" "$LOG" "systemctl restart sag-inference"
absent "it does not rely on enable --now"              "$LOG" "systemctl enable --now"

# --- which build a card asks for --------------------------------------------
#
# THE OTHER ONE. The installer carried its own copy of release.sh's matrix, and
# a card missing from it fell through to a plain `cuda` tarball that no
# invocation of release.sh has ever produced. Every case here was untested.
check "a card we build for installs"           "$LOG" "CUDA86-INSTALLED"

# A machine with no init system, which is how most rented GPUs arrive. The
# check used to be whether the systemctl BINARY existed, which plenty of
# images ship without being booted with systemd, so the install ran to the
# end and then could not start anything.
absent "no init is not a failed install"       "$LOG" "Can.t operate"
check "it says the service will not come back" "$LOG" "not running systemd"
absent "it needs no procps to know it started" "$LOG" "pgrep: not found"
check "and leaves a way to start it"           "$LOG" "START-SCRIPT-PRESENT"

# A machine added by certificate names no gateway, so the release URL cannot be
# built from one. It was, and the download became a path with no host.
absent "a download address always has a host"  "$LOG" "No host part in the URL"
absent "and a network fault is not blamed on the card" "$LOG" \
	"A10G is supported, but there is no build"
absent "it never asks for a generic cuda build" "$LOG" "x86_64-cuda\.tar\.gz"

# A refusal names the CARD, because "compute capability 75" is a number nobody
# bought and cannot act on. Both refusals below are reached without writing
# anything, so the machine is left clean either way.
check "an old card is named, not numbered"     "$LOG" "Tesla T4 is not supported"
check "and told which generation it is"        "$LOG" "Turing generation"
check "and why no build would help"            "$LOG" "bfloat16"
check "and what does work"                     "$LOG" "L4, L40, L40S"
check "and the cheap way in"                   "$LOG" "cheapest that run well"
# No price anywhere in a refusal. One was quoted, and an installer is the worst
# place to carry a number nothing will ever come back and correct.
absent "it quotes no price"                    "$LOG" "\\\$[0-9]"
check "and how to proceed without a GPU"       "$LOG" "accel cpu"

# The other refusal, which must NOT read like the first one.
check "a newer card is told the gap is ours"   "$LOG" "no build published for it yet"
check "and is shown what we do publish"        "$LOG" "We publish:.*86"
absent "it is never told to buy another card"  "$LOG" "Nextgen 900 is not supported"

check "it writes the address it was given"    "$LOG" "SAG_URL=https://sag.example.com"
check "it writes the port it listens on"      "$LOG" "SAG_NODE_ADDR=0.0.0.0:19443"
check "it writes where models are kept"       "$LOG" "SAG_NODE_DATA=/var/lib/sag-inference"
absent "it sets no address by hand"           "$LOG" "SAG_NODE_ADVERTISE="

# An upgrade must not overwrite what a machine has been configured with, or
# every upgrade is a re-configuration somebody has to redo.
check "an upgrade keeps the settings that were there" "$LOG" "SAG_MARKER=kept-from-before"

say ""
if [ "$FAILED" -eq 0 ]; then
	say "packaging: all checks passed"
else
	say "packaging: FAILED"
fi
exit "$FAILED"
