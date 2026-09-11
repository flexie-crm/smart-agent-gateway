#!/bin/sh
# Does systemd actually run it, keep it running, and give up when it should?
#
# The other gate (packaging_test.sh) proves the unit is VALID and the installer
# does the right things. Neither of those boots anything, and every fault in this
# area so far has only shown up when something did:
#
#   - a restart limit in the wrong section, which systemd ignored;
#   - an upgrade that reported success and changed nothing;
#   - a node that exited at startup because a download client would not build.
#
# So this runs a real init, installs the real tarball, and watches.
#
#   ./boot_test.sh                       # builds a tarball first
#   ./boot_test.sh path/to/node.tar.gz   # tests one that already exists
#
# Linux only, and needs a privileged container to run systemd as PID 1.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
NAME=sag-boot-test
FAILED=0
ok() { echo "  ok   $*"; }
bad() { echo "  FAIL $*"; FAILED=1; }

TARBALL="${1:-}"
if [ -z "$TARBALL" ]; then
	echo "==> building a tarball to install"
	"$HERE/release.sh" cpu "$HERE/../dist" >/dev/null
	TARBALL="$HERE/../dist/sag-inference-linux-x86_64-cpu.tar.gz"
fi
[ -f "$TARBALL" ] || { echo "boot: no tarball at $TARBALL" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT
cp "$TARBALL" "$WORK/node.tar.gz"
cp "$HERE/install.sh" "$WORK/"

echo "==> booting a machine with a real init"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --privileged --cgroupns=host \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
	-v "$WORK:/boot:ro" \
	debian:bookworm-slim /bin/sh -c \
	'apt-get update >/dev/null 2>&1 &&
	 apt-get install -y --no-install-recommends systemd systemd-sysv ca-certificates >/dev/null 2>&1 &&
	 exec /lib/systemd/systemd' >/dev/null

# `is-system-running` EXITS NON-ZERO for `degraded`, and degraded is the normal
# state of systemd in a container: some units it expects on real hardware are not
# there. So the word is what is read, not the exit code, or the test never gets
# past a machine that is perfectly well.
i=0
while [ "$i" -lt 90 ]; do
	case "$(docker exec "$NAME" systemctl is-system-running 2>&1 || true)" in
		running | degraded) break ;;
	esac
	i=$((i + 1))
	sleep 2
done
case "$(docker exec "$NAME" systemctl is-system-running 2>&1 || true)" in
	running | degraded) ;;
	*) echo "boot: systemd never came up" >&2; docker logs "$NAME" 2>&1 | tail -5; exit 1 ;;
esac

# A gateway that does not exist. The node cannot register, which is the ordinary
# state of a machine racked before its gateway is reachable, and it must serve
# and keep trying rather than exit.
echo "==> installing, pointed at a gateway that is not there"
docker exec "$NAME" sh -c 'cp /boot/install.sh /boot/node.tar.gz /root/ && cd /root &&
	sh install.sh --url https://nowhere.invalid --token abc_secret \
		--from ./node.tar.gz --accel cpu' >/dev/null 2>&1 || true

sleep 5
STATE=$(docker exec "$NAME" systemctl is-active sag-inference 2>&1 || true)
case "$STATE" in
	active) ok "it is running, with no gateway to register with" ;;
	*) bad "it is $STATE with no gateway; a machine must serve what it has and keep trying"
	   docker exec "$NAME" journalctl -u sag-inference -n 15 --no-pager 2>&1 | tail -15 ;;
esac

# It does NOT listen, and that is the design rather than a fault: a machine serves
# over a channel both ends authenticate, and this one has never registered, so it
# holds no certificate and there is no unencrypted way to reach it (KB/35). What
# matters is that it STAYS UP and keeps trying, which is what a machine racked
# before its gateway is reachable has to do. Asserting a port here asserted
# something that cannot exist yet.
#
# That it listens once enrolled is proved on the deployment itself, where a real
# gateway issues a real certificate, and not here.
if docker exec "$NAME" journalctl -u sag-inference --no-pager 2>&1 | grep -q "registering before it listens"; then
	ok "with no certificate it waits to be given one rather than serving unencrypted"
else
	bad "it did not say it was waiting for a certificate"
fi

echo "==> killing it, which is what a supervisor is for"
BEFORE=$(docker exec "$NAME" systemctl show sag-inference -p MainPID --value)
docker exec "$NAME" sh -c "kill -9 $BEFORE" 2>/dev/null || true
sleep 8
AFTER=$(docker exec "$NAME" systemctl show sag-inference -p MainPID --value)
if [ "$AFTER" != "$BEFORE" ] && [ "$AFTER" != "0" ]; then
	ok "it came back on its own after being killed"
else
	bad "it did not come back (was $BEFORE, now $AFTER)"
fi

# The escalation, which was silently not in force until systemd was asked.
LIMIT=$(docker exec "$NAME" systemctl show sag-inference -p StartLimitIntervalUSec --value)
BURST=$(docker exec "$NAME" systemctl show sag-inference -p StartLimitBurst --value)
[ "$LIMIT" = "1min" ] && ok "systemd is applying the restart interval ($LIMIT)" \
	|| bad "the restart interval is $LIMIT, not the 1min the unit asks for"
[ "$BURST" = "5" ] && ok "systemd is applying the restart burst ($BURST)" \
	|| bad "the restart burst is $BURST, not the 5 the unit asks for"

echo "==> upgrading, which is running the same command again"
OLD=$(docker exec "$NAME" systemctl show sag-inference -p MainPID --value)
docker exec "$NAME" sh -c 'cd /root && sh install.sh --url https://nowhere.invalid \
	--token another_secret --from ./node.tar.gz --accel cpu' >/dev/null 2>&1 || true
sleep 5
NEW=$(docker exec "$NAME" systemctl show sag-inference -p MainPID --value)
if [ "$NEW" != "$OLD" ] && [ "$NEW" != "0" ]; then
	ok "an upgrade actually replaced the running process"
else
	bad "an upgrade left the old process running (was $OLD, now $NEW)"
fi

echo "==> stopping it"
docker exec "$NAME" systemctl stop sag-inference >/dev/null 2>&1 || true
sleep 2
[ "$(docker exec "$NAME" systemctl is-active sag-inference 2>&1 || true)" = "inactive" ] \
	&& ok "it stops when it is asked to" || bad "it did not stop cleanly"

echo ""
[ "$FAILED" -eq 0 ] && echo "boot: all checks passed" || echo "boot: FAILED"
exit "$FAILED"
