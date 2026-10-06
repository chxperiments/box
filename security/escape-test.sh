#!/usr/bin/env bash
# Escape tests: run the things a hostile agent would try from inside a
# sandbox, and check that each one fails.
#
#   security/escape-test.sh [path/to/box] [standard|strict] [podman|krun|firecracker]
#
# firecracker sandboxes have no network and no host mounts, so the checks
# that need those report what they can and skip the rest.
#
# The second argument picks the Boxfile's isolation (default strict). Under
# standard the VMM runs as your own UID by design, which is reported but not
# counted as a failure.
#
# Uses a private BOX_HOME and a throwaway sandbox; your own sandboxes are
# not touched. Needs podman with krun, python3 on the host, and a static
# box (CGO_ENABLED=0). Exits non-zero if any escape succeeds.
set -u

BB=${1:-$(command -v box)}
ISOLATION=${2:-strict}
BACKEND=${3:-podman}
[ -x "$BB" ] || { echo "no box binary: $BB" >&2; exit 2; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/bb-escape.XXXXXX")
export BOX_HOME="$WORK/home"
NAME=escape
FAILS=0
PROBE_PID=""

cleanup() {
  [ -n "$PROBE_PID" ] && kill "$PROBE_PID" 2>/dev/null
  "$BB" destroy "$NAME" --data -y >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILS=$((FAILS + 1)); }
# in runs a shell line in the running sandbox; output on stdout, status kept.
guest() { "$BB" exec "$NAME" -- sh -c "$1" 2>&1; }

# ── Fixtures ────────────────────────────────────────────────────────────
mkdir -p "$WORK/ro" "$WORK/rw" "$WORK/secret"
# Under strict the sandbox is not you, so what it may read must be readable
# by others; the secret stays private, as a real one would be.
chmod 755 "$WORK" "$WORK/ro"; chmod 700 "$WORK/secret"
echo "host-secret-$$" > "$WORK/secret/key"
echo "read-only" > "$WORK/ro/file"
SECRET=$(cat "$WORK/secret/key")

# A service on the host's loopback, the kind an agent should never reach:
# a dev database, a docker API on TCP, another sandbox's agent port.
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
mkdir -p "$WORK/www" && echo "$SECRET" > "$WORK/www/index.html"
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$WORK/www" >/dev/null 2>&1 &
PROBE_PID=$!

"$BB" new "$NAME" >/dev/null
if [ "$BACKEND" = firecracker ]; then
cat > "$BOX_HOME/sandboxes/$NAME/Boxfile" <<EOF
base: docker.io/library/alpine:latest
cpus: 1
ram_mib: 512
network: none
readonly: true
isolation: $ISOLATION
backend: firecracker
EOF
else
cat > "$BOX_HOME/sandboxes/$NAME/Boxfile" <<EOF
base: docker.io/library/alpine:latest
cpus: 1
ram_mib: 512
network: bridge
readonly: true
isolation: $ISOLATION
backend: $BACKEND
mounts:
  - host: $WORK/ro
    guest: /ro
EOF
# strict allows read-only mounts only; standard also gets a writable one.
[ "$ISOLATION" = standard ] && cat >> "$BOX_HOME/sandboxes/$NAME/Boxfile" <<EOF
  - host: $WORK/rw
    guest: /rw
    mode: rw
EOF
fi

echo "building the test sandbox..."
"$BB" build "$NAME" >/dev/null 2>&1 || { echo "build failed" >&2; exit 2; }
"$BB" up "$NAME" >/dev/null || { echo "up failed" >&2; exit 2; }
sleep 0.5

echo
echo "kernel"
host_k=$(uname -r)
guest_k=$(guest 'uname -r')
[ "$guest_k" != "$host_k" ] && pass "guest runs its own kernel ($guest_k, host $host_k)" \
  || fail "guest reports the host kernel $host_k"

echo
echo "host network"
gw=$(guest "ip route 2>/dev/null | awk '/default/{print \$3; exit}'")
for target in 127.0.0.1 localhost host.containers.internal host.docker.internal 169.254.1.2 10.0.2.2 ${gw:+$gw}; do
  got=$(guest "wget -q -T 3 -O- http://$target:$PORT/ 2>/dev/null")
  if [ "$got" = "$SECRET" ]; then
    fail "reached a host-loopback service via $target:$PORT"
  else
    pass "no host-loopback service via $target"
  fi
done

echo
echo "host files"
got=$(guest "cat '$WORK/secret/key' 2>/dev/null; ls /home 2>/dev/null; cat /etc/hostname")
case "$got" in *"$SECRET"*) fail "read a host file outside the declared mounts" ;; *) pass "host paths outside mounts are not visible" ;; esac
SHARED=/rw; [ "$ISOLATION" = strict ] && SHARED=/data
got=$(guest "ln -sf '$WORK/secret/key' $SHARED/link && cat $SHARED/link 2>/dev/null")
case "$got" in *"$SECRET"*) fail "a symlink in a shared dir resolved onto the host" ;; *) pass "symlinks in shared dirs resolve inside the guest" ;; esac
got=$(guest "cat /rw/../../../../../../'$WORK'/secret/key 2>/dev/null; cat /data/../../../../'$WORK'/secret/key 2>/dev/null")
case "$got" in *"$SECRET"*) fail "path traversal out of a shared dir" ;; *) pass "no path traversal out of shared dirs" ;; esac
if [ -L "$WORK/rw/link" ] || [ -L "$BOX_HOME/data/$NAME/link" ]; then
  # The guest can plant a symlink pointing anywhere; that is only harmless
  # if host-side tooling never follows it. Record it so it stays visible.
  pass "guest-planted symlink exists on the host as a plain symlink (host tools must not follow it)"
fi

echo
echo "read-only boundaries"
[ "$BACKEND" != firecracker ] && { guest 'echo x > /ro/new' >/dev/null && [ -e "$WORK/ro/new" ] && fail "wrote through a read-only mount" || pass "read-only mount refuses writes"; }
[ "$BACKEND" != firecracker ] && { guest 'mount -o remount,rw /ro 2>/dev/null; echo x > /ro/new2' >/dev/null; [ -e "$WORK/ro/new2" ] && fail "remounted a read-only mount writable" || pass "remounting a read-only mount rw does not reach the host"; }
guest 'echo x > /etc/pwned' >/dev/null && fail "wrote to the read-only root" || pass "read-only root refuses writes"
if [ "$ISOLATION" = standard ] && [ "$BACKEND" != firecracker ]; then
  echo x > "$WORK/rw/probe"; [ "$(guest 'cat /rw/probe')" = "x" ] && pass "rw mount works (sanity)" || fail "rw mount unusable"
fi
[ "$BACKEND" != firecracker ] && { [ "$(guest 'cat /ro/file')" = "read-only" ] && pass "ro mount readable (sanity)" || fail "ro mount unreadable"; }
[ "$(guest 'echo ok > /data/w && cat /data/w')" = "ok" ] && pass "/data writable (sanity)" || fail "/data unusable"

echo
echo "the agent channel"
got=$(guest 'cat /proc/1/environ 2>/dev/null | tr "\0" "\n"; env; cat /.box/* 2>/dev/null | head -c 0')
case "$got" in *BOX_AGENT_TOKEN*) fail "the agent token is visible inside the guest" ;; *) pass "agent token not visible in the guest" ;; esac
guest 'echo x > /.box/box' >/dev/null && fail "guest can overwrite the agent binary" || pass "agent binary is read-only"
sock=$("$BB" env "$NAME" >/dev/null; ls "$BOX_HOME"/box.sock 2>/dev/null)
got=$(guest "ls -la / /run /tmp 2>/dev/null | grep -c box.sock")
[ "${got:-0}" = "0" ] && pass "SDK server socket is not exposed to the guest" || fail "SDK server socket visible in the guest"

echo
echo "devices and privileges"
guest '[ -e /dev/kvm ]' >/dev/null && fail "/dev/kvm is exposed to the guest (nested VMs)" || pass "no /dev/kvm in the guest"
if [ "$BACKEND" = firecracker ]; then
  vmm=$(cat "$BOX_HOME/vms/box-up-$NAME/pid" 2>/dev/null)
elif [ "$BACKEND" = krun ]; then
  vmm=$(krun --root "${XDG_RUNTIME_DIR:-/tmp}/box/krun" state "box-up-$NAME" 2>/dev/null | sed -n 's/.*"pid": *\([0-9]*\).*/\1/p')
else
  vmm=$(podman inspect "box-up-$NAME" --format '{{.State.Pid}}' 2>/dev/null)
fi
if [ -n "$vmm" ] && [ -r "/proc/$vmm/status" ]; then
  nnp=$(awk '/NoNewPrivs/{print $2}' "/proc/$vmm/status")
  sec=$(awk '/^Seccomp:/{print $2}' "/proc/$vmm/status")
  cap=$(awk '/CapEff/{print $2}' "/proc/$vmm/status")
  uid=$(awk '/^Uid:/{print $2}' "/proc/$vmm/status")
  [ "$nnp" = "1" ] && pass "VMM has no_new_privs" || fail "VMM lacks no_new_privs"
  [ "$sec" = "2" ] && pass "VMM runs under a seccomp filter" || fail "VMM has no seccomp filter"
  # CHOWN DAC_OVERRIDE FOWNER SETGID SETUID NET_BIND_SERVICE: what virtiofs
  # and low ports need, and nothing else.
  want=00000000000004cb; what="only the 6 capabilities it needs"
  [ "$BACKEND" = firecracker ] && want=0000000000000000 && what="no capabilities at all"
  [ "$cap" = "$want" ] && pass "VMM holds $what" \
    || fail "VMM holds unexpected capabilities ($cap, want $want)"
  if [ "$uid" != "$(id -u)" ]; then
    pass "VMM runs as host UID $uid, not yours"
  elif [ "$ISOLATION" = strict ]; then
    fail "VMM runs as your own UID $uid under strict isolation"
  else
    warn "VMM runs as your own UID $uid (standard isolation; use isolation: strict for agents)"
  fi
  [ "$(readlink /proc/$vmm/ns/net)" != "$(readlink /proc/self/ns/net)" ] && pass "VMM is in its own network namespace" \
    || fail "VMM shares the host network namespace"
else
  fail "could not inspect the VMM process"
fi

echo
echo "resource limits"
if [ "$BACKEND" != podman ] && [ -n "$vmm" ]; then
  cg=/sys/fs/cgroup$(sed -n 's/^0:://p' "/proc/$vmm/cgroup")
  pids=$(cat "$cg/pids.max" 2>/dev/null); mem=$(cat "$cg/memory.max" 2>/dev/null)
  [ "$pids" = max ] && pids=0; [ "$mem" = max ] && mem=0
else
  pids=$(podman inspect "box-up-$NAME" --format '{{.HostConfig.PidsLimit}}' 2>/dev/null)
  mem=$(podman inspect "box-up-$NAME" --format '{{.HostConfig.Memory}}' 2>/dev/null)
fi
[ "${pids:-0}" -gt 0 ] && pass "VMM has a pids limit ($pids)" || fail "VMM has no pids limit"
[ "${mem:-0}" -gt 0 ] && pass "VMM has a memory limit ($((mem / 1048576)) MiB)" || fail "VMM has no memory limit"
# A fork bomb saturates the guest's own kernel; the host must not notice.
timeout 5 "$BB" exec "$NAME" -- sh -c ':(){ :|:& };:' >/dev/null 2>&1
t0=$(date +%s%N); ls / >/dev/null; t1=$(date +%s%N)
[ $(( (t1 - t0) / 1000000 )) -lt 1000 ] && pass "host responsive during a guest fork bomb" \
  || fail "host slowed by a guest fork bomb"

echo
if [ "$FAILS" -eq 0 ]; then
  echo "no escapes."
else
  echo "$FAILS check(s) failed."
fi
exit $((FAILS > 0))
