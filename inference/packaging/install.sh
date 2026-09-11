#!/bin/sh
# Install the SAG inference node on a Linux machine.
#
#   curl -fsSL <url>/install.sh | \
#       sudo sh -s -- --url https://sag.example.com --token <token>
#
# The console prints the whole line under Machines. The token belongs to whoever
# asked for it, stands for an hour, and the machine that uses it destroys it, so
# what is left in a scrollback afterwards is dead.
#
# Or, with the tarball already on the box:
#
#   sudo ./install.sh --from ./sag-inference-linux-x86_64-cuda86.tar.gz --url ... --token ...
#
# It installs a BINARY. Nothing is compiled here: the engine takes twenty
# minutes to build and a GPU server is not the place to discover that.
#
# Running it again on an installed machine upgrades the binary and leaves the
# identity, the models and the settings alone.
set -eu

URL=""; TOKEN=""; ADVERTISE=""; NAME=""; FROM=""; ACCEL="cuda"; GATEWAY_CERT=""; NODE_KEY=""
PORT="19443"
ADDR=""
DATA="/var/lib/sag-inference"; DATA_GIVEN=""
# Where the binary comes from: the SAG server this machine is joining.
# Set once --url is known, because the answer is "from the deployment you are
# joining" rather than from anywhere of ours. A customer on a closed network
# reaches their own server by definition; they may not reach github.com, and an
# installer that needs the public internet is not an installer they can use.
RELEASES="${SAG_RELEASES:-}"

usage() {
	cat >&2 <<USAGE
usage: install.sh --url <gateway> --token <join token> [options]

  --url        the SAG server this machine joins, e.g. https://sag.example.com
  --token      the join token, from the console under Machines
  --advertise  rarely needed. This machine tells the gateway which port it
               listens on, and the gateway uses the address it saw the machine
               connect from, so an address is worked out on its own. Set this
               only when that is not where the gateway can reach it: a port
               forward that changes the port, or a proxy in front. Example:
               https://gpu1.example.com:19443
  --name       what to call it in the console (default: this machine's hostname)
  --accel      cuda (default) or cpu
  --gateway-cert  the certificate the console shows, for a machine that cannot
               reach the gateway to register itself. This machine will then
               accept that gateway and refuse everybody else.
  --port       what to listen on (default $PORT). Pick another if this one is
               taken, and open it on the firewall.
  --addr       host and port, when --port is not enough (an interface to bind to
               rather than all of them)
  --data       where models are kept (default $DATA). Point this at the big disk.
  --from       a tarball on this machine, instead of downloading one
USAGE
	exit 2
}

while [ $# -gt 0 ]; do
	case "$1" in
	--url) URL="${2:-}"; shift 2 ;;
	--token) TOKEN="${2:-}"; shift 2 ;;
	--advertise) ADVERTISE="${2:-}"; shift 2 ;;
	--name) NAME="${2:-}"; shift 2 ;;
	--accel) ACCEL="${2:-}"; shift 2 ;;
	--port) PORT="${2:-}"; shift 2 ;;
	--addr) ADDR="${2:-}"; shift 2 ;;
	--data) DATA="${2:-}"; DATA_GIVEN=yes; shift 2 ;;
	--from) FROM="${2:-}"; shift 2 ;;
	-h|--help) usage ;;
	--gateway-cert) GATEWAY_CERT="${2:-}"; shift 2 ;;
	--node-key) NODE_KEY="${2:-}"; shift 2 ;;
	*) echo "unknown option: $1" >&2; usage ;;
	esac
done

fail() { echo "install: $*" >&2; exit 1; }

# Everything that must be true is checked BEFORE anything is written. A
# half-installed machine is worse than one that refused, because it looks
# installed.
[ "$(id -u)" = 0 ] || fail "run this with sudo: it installs a system service"
# Two ways in, and they need different things.
#
# JOINING: the machine registers itself, so it needs somewhere to register and
# something to prove it may. That is the normal case and the better one, because
# nobody types an address and the gateway writes down the address it really saw.
#
# BY CERTIFICATE: the gateway cannot be called back (one on a laptop behind a
# router, or inside a company network with the machine rented outside it), so
# the two ends swap certificates by hand. What is pasted in is the gateway's,
# which is the only caller this machine will then accept; what goes back is this
# machine's own, printed at the end. There is nothing to register with and no
# token to spend, so demanding either would be asking for a credential we have
# already decided not to use.
# An UPGRADE needs no credentials at all.
#
# A machine that is already set up holds its identity, its key and its
# certificates on disk, and running the installer again to get a newer binary
# should not ask for any of it. It used to demand --url and --token, which a
# machine added by certificate has never had, so the only way to update one was
# to add it again from scratch: that mints a NEW key, which the machine then has
# and the gateway does not, and the machine goes silent.
#
# Where its data lives is read from the settings rather than asked for again,
# because --data was answered once and a person upgrading should not have to
# remember what they said.
if [ -z "$DATA_GIVEN" ] && [ -r /etc/sag-inference/node.env ]; then
	FROM_SETTINGS=$(sed -n 's/^SAG_NODE_DATA=//p' /etc/sag-inference/node.env | tail -1)
	[ -n "$FROM_SETTINGS" ] && DATA="$FROM_SETTINGS"
fi
ENROLLED=no
if [ -s "$DATA/identity.json" ] && [ -s "$DATA/node-cert.pem" ] && [ -s "$DATA/authority.pem" ]; then
	ENROLLED=yes
fi

if [ -n "$GATEWAY_CERT" ]; then
	[ -z "$TOKEN" ] || fail "--gateway-cert is for a machine that cannot register itself, so --token is not used with it"
	# --url stays allowed and optional. A machine that CAN reach the gateway
	# still checks in daily to renew its certificate; one that cannot simply
	# never does, and is re-enrolled before the certificate runs out.
elif [ "$ENROLLED" = yes ]; then
	# Already set up. This is an upgrade and nothing about its identity changes.
	echo "==> this machine is already set up, so this is an upgrade"
else
	[ -n "$URL" ] || fail "--url is required (the SAG server this machine joins)"
	[ -n "$TOKEN" ] || fail "--token is required (the console shows one under Inference)"
fi
if [ -n "$URL" ]; then
	case "$URL" in https://*|http://*) ;; *) fail "--url must start with https:// or http://" ;; esac
fi
case "$ACCEL" in
	cpu | cuda | cuda[0-9]*) ;;
	*) fail "--accel must be cpu, cuda, or a capability such as cuda86" ;;
esac

# --port is the usual way and --addr is the escape hatch, so one is derived from
# the other rather than both being written and one silently winning.
case "$PORT" in
	''|*[!0-9]*) fail "--port must be a number" ;;
	*) [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || fail "--port must be between 1 and 65535" ;;
esac
[ -n "$ADDR" ] || ADDR="0.0.0.0:$PORT"
# HOW this machine will keep the service running, decided rather than assumed.
#
# This was `command -v systemctl || fail`, which asks whether the BINARY EXISTS.
# That is not the question. Plenty of container images ship systemctl and are not
# booted with systemd, and there the install ran all the way to the end, wrote
# everything, and then said "System has not been booted with systemd as init
# system (PID 1). Can't operate." having already installed the binary, the
# certificates and the settings.
#
# The canonical test is whether systemd is actually running, which is the
# existence of /run/systemd/system, not the presence of a command. And a machine
# without it is not a machine that cannot run this: rented GPUs are very often
# containers with no init at all. So it selects a way to run instead of refusing.
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
	SUPERVISOR=systemd
else
	SUPERVISOR=none
fi
[ "$(uname -s)" = Linux ] || fail "this installer is for Linux"
[ "$(uname -m)" = x86_64 ] || fail "only x86_64 is published today; this machine is $(uname -m)"

# A CUDA build against a box with no driver produces a process that starts and
# then cannot load a single model, which is a confusing way to fail an hour
# later. Say it now.
if [ "$ACCEL" = cuda ] && ! command -v nvidia-smi >/dev/null 2>&1; then
	fail "no NVIDIA driver found (nvidia-smi is missing). Install the driver, or pass --accel cpu"
fi

# Where the binaries come from.
#
# A machine that JOINS downloads from the deployment it is joining, which is the
# whole point: a rack on a closed network reaches its own server by definition
# and may reach nothing else.
#
# A machine added BY CERTIFICATE has no gateway address at all, because the
# gateway is the thing that cannot be reached. Falling through to
# "$URL/inference/download/v1" with URL empty produced `/inference/download/v1`,
# which curl rejects for having no host, and the failure was then reported as
# "there is no build published for your card" about a card we publish for.
if [ -z "$RELEASES" ]; then
	if [ -n "$URL" ]; then
		RELEASES="$URL/inference/download/v1"
	else
		RELEASES="https://sag-repo.flexie.io/inference/download/v1"
	fi
fi
# Which build this machine needs, asked of the machine.
#
# A CUDA binary is compiled for one GPU architecture and will not start on
# another: run an Ampere build on a Blackwell card and it fails at load with
# something that reads like a broken install. The card knows which it is, so
# nobody should have to.
#
# There is NO list of known capabilities here, deliberately. There used to be,
# and it was a second copy of `release.sh`'s matrix that nothing kept in step:
# publishing a new architecture meant editing two files, and a card missing from
# this one fell through to a plain `cuda` tarball that no invocation of
# `release.sh` has ever produced.
#
# What IS here is the engine's floor, which is a different fact from the build
# matrix and changes for a different reason: the engine needs bfloat16, which
# arrived with the Ampere generation, so a card below 8.0 cannot run it at all
# and no build of ours would change that.
ENGINE_FLOOR=80

# The two ways this can end badly, and they are NOT the same answer.
#
# A card below the floor is a permanent no and the owner needs to know what to
# buy or rent instead. A card above it that we have not published for is a
# temporary no and the answer is to ask us. Telling somebody with a B300 to go
# and buy an A100 would be absurd, and one shared "unsupported" message does
# exactly that.
supported_cards() {
	echo "         Ampere    A100, A30, A10, A10G, A40, RTX 3090, RTX A6000" >&2
	echo "         Ada       L4, L40, L40S, RTX 4090, RTX 6000 Ada" >&2
	echo "         Hopper    H100, H200, GH200" >&2
	echo "         Blackwell B200, GB200, B300, GB300, RTX 5090, RTX PRO 6000" >&2
}
too_old() {
	echo "" >&2
	echo "install: $1 is not supported." >&2
	echo "" >&2
	echo "         It is a $2 card, and this engine needs an Ampere generation" >&2
	echo "         card or newer (2020 and later). The reason is bfloat16, which" >&2
	echo "         older cards do not have in hardware, so there is no build of" >&2
	echo "         ours that would make this one work." >&2
	echo "" >&2
	echo "         Cards that do work:" >&2
	supported_cards
	echo "" >&2
	echo "         The cheapest that run well are the L4 and the A10. No price is" >&2
	echo "         quoted here on purpose: it would be wrong within months and" >&2
	echo "         nothing would correct it." >&2
	echo "" >&2
	echo "         To run on this machine without a GPU instead, add --accel cpu." >&2
	echo "         It works, and it is a great deal slower." >&2
	exit 1
}
# Named for the generation rather than the number, because that is how the card
# was sold. Anything at or above the floor is handled by no_build_for instead.
generation_of() {
	case "$1" in
		70 | 72) echo "Volta generation, 2017" ;;
		75) echo "Turing generation, 2018" ;;
		*) echo "pre-Ampere" ;;
	esac
}
# Above the floor, but nothing published. The card is fine; we are behind.
no_build_for() {
	_have=$(curl -fsSL "$RELEASES/builds.txt" 2>/dev/null | sed "s/^cuda//" | tr "\n" " ")
	echo "" >&2
	echo "install: $1 is supported, but there is no build published for it yet." >&2
	echo "" >&2
	echo "         Its compute capability is $2. We publish: ${_have:-unknown}" >&2
	echo "" >&2
	echo "         This is our gap and not a limit of the card. Tell us which card" >&2
	echo "         this is and we will publish a build for it." >&2
	echo "" >&2
	echo "         To run on this machine without a GPU meanwhile, add --accel cpu." >&2
	exit 1
}

if [ "$ACCEL" = cuda ]; then
	CARD=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)
	CAP=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader 2>/dev/null | head -1 | tr -d " .")
	[ -n "$CAP" ] || fail "could not ask this machine what graphics card it has (nvidia-smi). Install the driver, or pass --accel cpu"
	[ -n "$CARD" ] || CARD="This card"
	# 86 reads back as 8.6, which is how NVIDIA writes it and how anybody
	# searching for it will have seen it.
	echo "==> found $CARD (compute capability $(printf %s "$CAP" | sed "s/\(.\)\$/.\1/"))"
	# The floor is checked HERE rather than after a failed download, because an
	# old card is a fact about the card and does not depend on what we publish.
	if [ "$CAP" -lt "$ENGINE_FLOOR" ]; then
		too_old "$CARD" "$(generation_of "$CAP")"
	fi
	ACCEL="cuda$CAP"
fi

TARBALL="sag-inference-linux-x86_64-$ACCEL.tar.gz"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -n "$FROM" ]; then
	[ -f "$FROM" ] || fail "no such file: $FROM"
	cp "$FROM" "$WORK/$TARBALL"
else
	echo "==> downloading $TARBALL"
	command -v curl >/dev/null 2>&1 || fail "curl is needed to download the release"
	# The status is captured through `if`, not by reading $? on the next line.
	# `set -e` exits on a failing command that is not a condition, so the
	# assignment was never reached and a failed download killed the script in
	# silence: no message, no exit code anybody could read.
	if curl -fsSL "$RELEASES/$TARBALL" -o "$WORK/$TARBALL"; then
		WHY=0
	else
		WHY=$?
	fi
	if [ "$WHY" != 0 ]; then
		# WHICH failure decides what to say, and conflating them told somebody
		# with an H200 that we publish no build for the H200. curl's exit 22 is
		# the server answering with an error, which for a capability we do not
		# publish is a 404 and is the only case that means "not your card". Every
		# other code is a fault of ours or of the network: 3 was a URL with no
		# host, 6 cannot resolve, 7 cannot connect.
		case "$WHY:$ACCEL" in
			22:cuda[0-9]*) no_build_for "${CARD:-This card}" "$(printf %s "${ACCEL#cuda}" | sed "s/\(.\)\$/.\1/")" ;;
			22:*) fail "there is no $ACCEL build published at $RELEASES" ;;
			3:*) fail "the download address is not a URL: $RELEASES/$TARBALL" ;;
			6:*) fail "the download host could not be resolved: $RELEASES" ;;
			7:*) fail "the download host could not be reached: $RELEASES" ;;
			*) fail "could not download $RELEASES/$TARBALL (curl exit $WHY)" ;;
		esac
	fi
fi

tar xzf "$WORK/$TARBALL" -C "$WORK" || fail "the archive could not be read"
[ -x "$WORK/sag-inference" ] || fail "the archive does not contain sag-inference"

# What this binary needs from the machine, asked BEFORE anything is installed.
#
# A CUDA build links against five libraries and ships none of them. One,
# libcuda.so.1, comes from the DRIVER and is on any machine with a working
# card. The other four are the CUDA RUNTIME, which arrives with the toolkit and
# is absent on a machine that has only drivers. They total 960 MB against a
# 50 MB binary, so shipping them is not a trade worth making.
#
# Without this the failure lands after everything is written and configured, as
# "error while loading shared libraries: libcudart.so.12" in a log, from a
# service that will not start and that a person then has to go and read. Asked
# here it costs nothing and names the exact thing to install.
if command -v ldd >/dev/null 2>&1; then
	MISSING=$(ldd "$WORK/sag-inference" 2>/dev/null | awk '/not found/ {print $1}' | tr '\n' ' ')
	if [ -n "$MISSING" ]; then
		echo >&2
		echo "install: this machine is missing libraries the node needs:" >&2
		echo >&2
		for lib in $MISSING; do echo "         $lib" >&2; done
		echo >&2
		case "$MISSING" in
			*libcuda.so.1*)
				echo "         libcuda.so.1 comes from the NVIDIA driver. This machine has a card" >&2
				echo "         but no working driver, so install that first." >&2
				;;
		esac
		case "$MISSING" in
			*libcudart*|*libcublas*|*libcurand*)
				echo "         The others are the CUDA runtime, which comes with the toolkit and" >&2
				echo "         is not part of the driver. On Debian or Ubuntu:" >&2
				echo >&2
				echo "             apt-get install -y cuda-runtime-12-8" >&2
				echo >&2
				echo "         or, on any distribution with python:" >&2
				echo >&2
				echo "             pip install nvidia-cuda-runtime-cu12 nvidia-cublas-cu12 nvidia-curand-cu12" >&2
				echo >&2
				echo "         and then run this installer again." >&2
				;;
		esac
		echo >&2
		echo "         To run on the processor instead, which needs none of them," >&2
		echo "         add --accel cpu." >&2
		exit 1
	fi
fi

# The account it runs as. No login, no home worth having: it owns its data
# directory and nothing else.
if ! id sag-inference >/dev/null 2>&1; then
	useradd --system --shell /usr/sbin/nologin --home-dir "$DATA" --no-create-home sag-inference \
		|| fail "could not create the sag-inference account"
fi

install -m 0755 "$WORK/sag-inference" /usr/local/bin/sag-inference
# The data directory, which is often not ours to own.
#
# `install -d -o ... -m ...` is right when we are creating it, and wrong when it
# already exists somewhere we do not control. Pointing --data at a mounted volume
# is the NORMAL case for a rented GPU machine, which is exactly why the option
# exists, and on many of them chown is not permitted at all: a container volume,
# an NFS or SMB mount, a filesystem mounted without the capability. It failed
# with "cannot change owner and permissions of /workspace: Operation not
# permitted" and stopped the install over something that does not matter.
#
# What matters is whether the service can WRITE there, so that is what is
# checked, and ownership is attempted rather than demanded.
if [ ! -d "$DATA" ]; then
	install -d -o sag-inference -g sag-inference -m 0750 "$DATA" \
		|| fail "could not make $DATA. Check the path, or point --data somewhere writable."
else
	chown sag-inference:sag-inference "$DATA" 2>/dev/null || true
	chmod 0750 "$DATA" 2>/dev/null || true
fi
# WHO the service runs as, decided by what the storage allows.
#
# Normally it is an account of its own, which is what an unprivileged service
# should be. On a volume that refuses chown, that account can never own its own
# files, and the private key would have to be made world readable for it to
# start: strictly worse than the alternative, which is to run as the account
# that already owns everything there.
#
# So the storage decides, and it is SAID rather than done quietly, because
# "which user is this service" is exactly the sort of thing somebody needs to
# know without reading a unit file.
RUN_AS="sag-inference"
if ! su -s /bin/sh sag-inference -c "test -w \"$DATA\"" 2>/dev/null; then
	RUN_AS="root"
	echo "==> $DATA does not allow this machine to give files to a service account,"
	echo "    which is normal for a mounted volume, so the service will run as root."
fi
install -d -m 0755 /etc/sag-inference

# An upgrade keeps what the machine already is. Its identity, its key and its
# models are on disk and none of them are this script's to replace.
if [ -f /etc/sag-inference/node.env ]; then
	echo "==> keeping the existing settings in /etc/sag-inference/node.env"
else
	umask 077
	cat > /etc/sag-inference/node.env <<ENV
# Written by install.sh. The join token is spent by the gateway the moment this
# machine registers, and is worthless from then on: every later check-in is
# signed with the key this machine minted for itself, under $DATA. So an upgrade
# does not need a new one, and the dead value below is not a secret.
SAG_NODE_ADDR=$ADDR
SAG_NODE_DATA=$DATA
SAG_NODE_NAME=${NAME:-$(hostname)}
SAG_NODE_LOG=sag_inference=info,warn
ENV
	# Written only when there is one, because an empty setting is not the same
	# as an absent one. A machine enrolled from the console has no gateway to
	# call and no token to spend, and `SAG_URL=` would make it look configured
	# to reach somewhere it cannot, which is the difference between "standalone
	# on purpose" and "somebody left this half done".
	[ -n "$URL" ] && echo "SAG_URL=$URL" >> /etc/sag-inference/node.env
	[ -n "$TOKEN" ] && echo "SAG_JOIN_TOKEN=$TOKEN" >> /etc/sag-inference/node.env
	# Only when given. Left out, the gateway uses the address it saw the join
	# arrive from, which is right on a private network and wrong behind NAT.
	[ -n "$ADVERTISE" ] && echo "SAG_NODE_ADVERTISE=$ADVERTISE" >> /etc/sag-inference/node.env
	chmod 0640 /etc/sag-inference/node.env
	chown root:sag-inference /etc/sag-inference/node.env
fi

# The half of the exchange that happens HERE.
#
# The console cannot be called back by this machine, so the two ends swap
# certificates by hand instead of over a join. What arrived is the gateway's,
# which becomes the only thing this machine will accept a caller from. What is
# generated here is this machine's own, over a key that is made on this disk and
# never leaves it, and which is printed at the end for pasting back.
#
# These are the same four files a joined machine ends up with, so the engine
# makes no distinction: finding a certificate on disk it listens at once and
# never registers (inference/src/main.rs, the `Some(held)` arm). Nothing on the
# machine had to learn about this way in.
#
# It overwrites, because re-running with a fresh certificate is how a machine
# whose gateway was rebuilt is repaired, and it cannot ask anybody.
if [ -n "$GATEWAY_CERT" ]; then
	command -v openssl >/dev/null 2>&1 \
		|| fail "openssl is needed to make this machine a certificate and is not installed"

	# It arrives base64 encoded, which is what makes it one word with no
	# newlines and no shell quoting to get wrong. PEM is still accepted, because
	# somebody pasting one by hand is a reasonable thing to do and refusing it
	# would be refusing the obvious.
	umask 077
	case "$GATEWAY_CERT" in
		*BEGIN\ CERTIFICATE*) printf '%b\n' "$GATEWAY_CERT" | sed -e 's/^[[:space:]]*//' > "$DATA/authority.pem" ;;
		*) printf %s "$GATEWAY_CERT" | base64 -d > "$DATA/authority.pem" 2>/dev/null \
			|| fail "that certificate could not be read. Copy the whole line from the console." ;;
	esac
	grep -q "BEGIN CERTIFICATE" "$DATA/authority.pem" \
		|| fail "that is not a certificate. Copy all of it from the console, including the BEGIN and END lines."
	openssl x509 -in "$DATA/authority.pem" -noout >/dev/null 2>&1 \
		|| fail "that certificate could not be read. Copy all of it from the console."

	# This machine's own, generated here. The address it is FOR is written into
	# it, so a certificate copied onto a different machine is of no use there.
	# The gateway holds these exact bytes and accepts nothing else, so the name
	# is for a person reading it rather than for verification.
	if [ ! -s "$DATA/node-key.pem" ] || [ ! -s "$DATA/node-cert.pem" ]; then
		echo "==> making this machine a certificate of its own"
		SUBJECT_IP=$(curl -fsSL --max-time 10 "https://sag-repo.flexie.io/v1/ip?plain=1" 2>/dev/null | tr -d '\r\n')
		[ -n "$SUBJECT_IP" ] || SUBJECT_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
		[ -n "$SUBJECT_IP" ] || SUBJECT_IP=127.0.0.1
		openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
			-keyout "$DATA/node-key.pem" -out "$DATA/node-cert.pem" \
			-days 3650 -subj "/CN=$(hostname)" \
			-addext "subjectAltName=IP:$SUBJECT_IP,DNS:$(hostname)" \
			-addext "extendedKeyUsage=serverAuth" >/dev/null 2>&1 \
			|| fail "this machine could not make itself a certificate (openssl failed)"
	else
		echo "==> keeping the certificate this machine already has"
	fi

	# An identity file, because the engine wants one and a machine added this
	# way is never asked its name by anybody: the certificate is what identifies
	# it. Minted here rather than given, so nothing about it has to travel.
	# The KEY here has to be the one the gateway holds. A node refuses any caller
	# whose bearer token does not match it (src/api/auth.rs), and it used to be
	# minted here while the gateway minted its own, so the two never matched and
	# every call was refused with 401 after a perfectly good TLS handshake. It
	# now arrives with the certificate, and is written even on a re-run, because
	# a machine being added again is being given a new one.
	if [ -n "$NODE_KEY" ]; then
		printf '{\n  "node_id": "nd_%s",\n  "key": "%s"\n}\n' \
			"$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
			"$NODE_KEY" > "$DATA/identity.json"
	elif [ ! -s "$DATA/identity.json" ]; then
		echo "==> WARNING: this command carries no key, so the gateway will be" >&2
		echo "    refused by this machine. Generate the command again." >&2
		printf '{\n  "node_id": "nd_%s",\n  "key": "%s"\n}\n' \
			"$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
			"$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" > "$DATA/identity.json"
	fi

	# Best effort, never fatal: on a volume that refuses it the files are
	# already owned by the account the service will run as (see RUN_AS above),
	# and a failed chown there stopped the install after it had written
	# everything, which is the worst place to stop.
	chown "$RUN_AS:$RUN_AS" "$DATA/identity.json" "$DATA/node-key.pem" \
		"$DATA/node-cert.pem" "$DATA/authority.pem" 2>/dev/null || true
	chmod 0600 "$DATA/identity.json" "$DATA/node-key.pem"
	chmod 0644 "$DATA/node-cert.pem" "$DATA/authority.pem"
fi

if [ "$SUPERVISOR" = systemd ]; then
	install -m 0644 "$WORK/sag-inference.service" /etc/systemd/system/sag-inference.service
	# The data directory may have been pointed somewhere else.
	sed -i "s|ReadWritePaths=/var/lib/sag-inference|ReadWritePaths=$DATA|" /etc/systemd/system/sag-inference.service
	sed -i "s|^User=sag-inference|User=$RUN_AS|; s|^Group=sag-inference|Group=$RUN_AS|" \
		/etc/systemd/system/sag-inference.service
	systemctl daemon-reload
fi

# A way to start it that does not depend on an init system.
#
# Written on EVERY machine, systemd or not. Where there is systemd it is what a
# person runs to check the thing by hand; where there is not, it is how the
# service is started, and it is what a container platform is pointed at to bring
# the node up with the box.
install -d -m 0755 /usr/local/lib/sag-inference 2>/dev/null || true
cat > /usr/local/bin/sag-inference-start <<'START'
#!/bin/sh
# Start the inference node with the settings the installer wrote.
#
# `set -a` before sourcing is what exports them. Sourcing alone sets variables
# in this shell and does NOT put them in the environment of the program, so the
# node would start with none of its settings and bind the wrong address. The
# alternative, exporting a list built by grepping the file for names, is one
# more thing to get wrong for no gain.
set -eu
set -a
. /etc/sag-inference/node.env
set +a
exec /usr/local/bin/sag-inference serve
START
chmod 0755 /usr/local/bin/sag-inference-start

# One way to control this machine's node, whatever runs it.
#
# On a box with systemd there is `systemctl restart sag-inference` and everybody
# knows it. On a container with no init there was nothing, so the answer to "how
# do I restart it" was a kill and a nohup typed by hand, which is not an answer.
#
# So there is one command with the verbs a service has, and on a systemd machine
# it hands straight over to systemctl rather than doing anything of its own: two
# mechanisms managing one process is how you end up with two of them running.
cat > /usr/local/bin/sag-inference-ctl <<'CTL'
#!/bin/sh
# Control the inference node: start, stop, restart, status, logs.
set -eu

PID_FILE=/run/sag-inference.pid
LOG_FILE=/var/log/sag-inference.log

# Which mechanism this machine actually uses, asked the same way the installer
# asks it: whether systemd is RUNNING, not whether systemctl exists.
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
	case "${1:-status}" in
		start|stop|restart|status) exec systemctl "$1" sag-inference ;;
		logs) exec journalctl -u sag-inference -f ;;
		*) echo "usage: sag-inference-ctl start|stop|restart|status|logs" >&2; exit 2 ;;
	esac
fi

running() {
	[ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE" 2>/dev/null)" 2>/dev/null
}

start() {
	if running; then echo "already running (pid $(cat $PID_FILE))"; return 0; fi
	mkdir -p /var/log /run
	nohup /usr/local/bin/sag-inference-start >"$LOG_FILE" 2>&1 &
	echo $! > "$PID_FILE"
	# Started is not running. Give it a moment and report what is true.
	sleep 2
	if running; then echo "started (pid $(cat $PID_FILE))"; else
		echo "it did not stay up. What it said:" >&2
		tail -n 20 "$LOG_FILE" >&2 2>/dev/null || true
		exit 1
	fi
}

stop() {
	if ! running; then echo "not running"; return 0; fi
	PID=$(cat "$PID_FILE")
	kill "$PID" 2>/dev/null || true
	# Asked to stop, then made to. A model being released can take a moment, so
	# it gets one before anything harsher.
	i=0
	while kill -0 "$PID" 2>/dev/null && [ $i -lt 15 ]; do sleep 1; i=$((i + 1)); done
	kill -9 "$PID" 2>/dev/null || true
	rm -f "$PID_FILE"
	echo "stopped"
}

case "${1:-status}" in
	start) start ;;
	stop) stop ;;
	restart) stop; start ;;
	status)
		if running; then echo "running (pid $(cat $PID_FILE))"; else echo "not running"; exit 3; fi
		;;
	logs) exec tail -f "$LOG_FILE" ;;
	*) echo "usage: sag-inference-ctl start|stop|restart|status|logs" >&2; exit 2 ;;
esac
CTL
chmod 0755 /usr/local/bin/sag-inference-ctl

# `enable` then `restart`, and NOT `enable --now`.
#
# `--now` starts a stopped service and does nothing at all to a running one. So
# running this again to upgrade wrote the new binary, wrote the new unit, and
# left the old process serving from the old image, while the check below found
# the service active and printed "Installed and running." An upgrade that reports
# success and changes nothing is worse than one that fails.
#
# `restart` is right in both cases: it starts a service that was not running and
# replaces one that was.
if [ "$SUPERVISOR" = systemd ]; then
	systemctl enable sag-inference >/dev/null 2>&1 \
		|| fail "the service could not be enabled; see: journalctl -u sag-inference"
	systemctl restart sag-inference \
		|| fail "the service could not be started; see: journalctl -u sag-inference"
else
	# No init to hand it to, so it is started here. A previous one is stopped
	# first, because running this again is how a machine is upgraded and two
	# copies would fight over the port.
	# The previous one, stopped by the pid we wrote rather than by pattern.
	#
	# pkill and pgrep are procps and are NOT on a minimal image: the gate found
	# "pgrep: not found", after which the installer decided a running node was
	# not running and reported a failed install. A pid file needs nothing that
	# is not already there, and is exact where a pattern match is a guess.
	/usr/local/bin/sag-inference-ctl stop >/dev/null 2>&1 || true
	/usr/local/bin/sag-inference-ctl start >/dev/null 2>&1 || true
fi

# Started is not running. Give it a moment and then report what is actually
# true, because "installed" and "working" are different claims.
if [ "$SUPERVISOR" = systemd ]; then
	LOGS="journalctl -u sag-inference -f"
else
	LOGS="/var/log/sag-inference.log"
fi

sleep 3
if { [ "$SUPERVISOR" = systemd ] && systemctl is-active --quiet sag-inference; } \
	|| { [ "$SUPERVISOR" = none ] && [ -f /run/sag-inference.pid ] \
		&& kill -0 "$(cat /run/sag-inference.pid)" 2>/dev/null; }; then
	cat <<DONE

  Installed and running.

    settings   /etc/sag-inference/node.env
    models     $DATA
    logs       $LOGS
DONE
	if [ "$SUPERVISOR" = none ]; then
		cat <<NOINIT

  This machine is not running systemd, which is normal for a rented GPU
  container, so there is nothing to register a service with. It has been
  started directly and will NOT come back on its own if the machine restarts.

  To control it:

      sag-inference-ctl restart
      sag-inference-ctl status
      sag-inference-ctl logs

  To have it start with the machine, set your provider's start command to:

      /usr/local/bin/sag-inference-start
NOINIT
	fi

	# Whether the outside can actually get in, asked of the outside.
	#
	# Everything visible from this machine now says it works: the service is
	# active, the port is bound, curl against localhost answers. None of that is
	# the question. SAG connects TO this machine, so what matters is whether a
	# packet from elsewhere arrives, and a closed security group, an unopened
	# firewall and a missing port forward all look exactly like this from here.
	# The machine cannot answer it; something outside has to try.
	#
	# Asked of wherever the installer came from, which is the one host we know
	# this machine can reach and which can therefore be asked to reach back. On a
	# closed network that is the deployment itself, which is the right answer
	# there too, because it is the party that has to make the connection. It is
	# never fatal: it is a report, and a machine can be perfectly installed and
	# deliberately not exposed.
	VERIFY="${RELEASES%/inference/download/v1}"
	case "$VERIFY" in
		https://*|http://*) ;;
		# Nothing to ask. An install from a local tarball with no gateway named
		# has no host to put the question to, and an empty address produces
		# "( did not answer)", which reads as a service being down rather than
		# as a question never asked.
		*) VERIFY="" ;;
	esac
	if [ -n "$VERIFY" ]; then
	# NOT `curl -f`. That turns any 4xx into an empty body and a non-zero exit,
	# so a service answering "your address is private, the internet cannot reach
	# you either way" arrives here indistinguishable from a host that is down,
	# and the installer reports the wrong thing. The body IS the answer, on
	# success and refusal alike, so it is read either way.
	REACH=$(curl -sS --max-time 30 "$VERIFY/v1/reachable?port=$PORT" 2>/dev/null || true)
	case "$REACH" in
		*'"reachable":true'*)
			SEEN=$(printf %s "$REACH" | sed -n 's/.*"address":"\([^"]*\)".*/\1/p')
			case "$REACH" in
				*'"tls":true'*)
					cat <<REACHED

  Reachable from the internet, and it answered as the node.

    address    https://$SEEN

  Add that address when you add this machine.
REACHED
					;;
				*)
					cat <<HALF

  Something answered on port $PORT from the internet, but it was not this node.
  Another service may already be using the port. Check what else is listening
  before adding this machine.
HALF
					;;
			esac
			;;
		*'"reachable":false'*)
			MYIP=$(printf %s "$REACH" | sed -n 's/.*"ip":"\([^"]*\)".*/\1/p')
			cat >&2 <<BLOCKED

  Not reachable from the internet on port $PORT.

  The service is running here, so this is the network in front of it: a firewall,
  a cloud security group, or a missing port forward. Nothing is wrong with the
  install.

    seen from outside as   ${MYIP:-unknown}
    needs to allow         inbound TCP $PORT

  Fix that and check again with:
    curl $VERIFY/v1/reachable?port=$PORT
BLOCKED
			;;
		*'"error"'*)
			# It answered and declined to test. The usual reason is a private
			# source address, which is a real answer about this machine and not
			# a failure of the check.
			WHY=$(printf %s "$REACH" | sed -n 's/.*"error":"\([^"]*\)".*/\1/p')
			cat >&2 <<REFUSED

  Could not check from outside: $WHY

  The install is fine. Verify the port yourself before adding this machine.
REFUSED
			;;
		*)
			cat <<UNKNOWN

  Could not check from outside whether this machine can be reached ($VERIFY did
  not answer). The install is fine; verify the port yourself before adding it.
UNKNOWN
			;;
	esac

	fi

	# What happens next, which is a different sentence for each way in.
	#
	# A joining machine puts itself on the list. An enrolled one is already on
	# it, because the gateway wrote the row when it minted the identity, and it
	# turns from off to on when the gateway can reach it. Telling somebody who
	# enrolled a machine that "it registers itself with " leaves them waiting for
	# something that is never going to happen.
	if [ -n "$GATEWAY_CERT" ]; then
		# The other half of the exchange, printed for copying back.
		#
		# Both together and clearly separated, because they are pasted into two
		# different boxes and the certificate is a wall of text: somebody
		# skimming for the address should not have to hunt for it underneath.
		# The address comes first for that reason, short and alone.
		WHERE=$(curl -fsSL --max-time 10 "https://sag-repo.flexie.io/v1/ip?plain=1" 2>/dev/null | tr -d '\r\n')
		[ -n "$WHERE" ] || WHERE=$(hostname -I 2>/dev/null | awk '{print $1}')
		cat <<HALF

  ==============================================================
   Now finish adding it in SAG. Two things to copy back:
  ==============================================================

  1. The address of this machine

       https://${WHERE:-THIS-MACHINE-IP}:$PORT

     That port is the one this machine LISTENS on. A rented GPU is often behind
     a mapping, and then the port reaching it from outside is a different number
     that only your provider knows: look for "exposed" or "public" ports there
     and use that one instead. The certificate below is good either way.

  2. This machine certificate

HALF
		sed 's/^/  /' "$DATA/node-cert.pem"
		cat <<TAIL

  Paste those two into the "Add a machine" window and press Add.
  Nothing else can talk to this machine: it now accepts only the
  gateway whose certificate you gave it.
TAIL
	else
		cat <<REGISTER

  It registers itself with $URL. Open the console, go to Inference, and it should
  be there within a few seconds. If it is not, the reason will be in the log.
REGISTER
	fi
else
	echo >&2
	echo "  Installed, but it is not running. What it said:" >&2
	# From wherever this machine actually keeps it. Asking journalctl on a box
	# with no journal prints nothing and hides the reason the install failed.
	if [ "$SUPERVISOR" = systemd ]; then
		journalctl -u sag-inference -n 20 --no-pager >&2 || true
	else
		tail -n 20 /var/log/sag-inference.log >&2 2>/dev/null || echo "  (no log was written)" >&2
	fi
	exit 1
fi
