#!/usr/bin/env bash
# Fork lifecycle against real microVMs: branch, run in the fork while the
# parent stays untouched, diff, apply, discard, the guards, and the symlink
# a guest plants in /data to try to turn apply into a write elsewhere.
#
#   test/fork-e2e.sh [path/to/box] [standard|strict]
#
# Uses a private BOX_HOME; needs podman with krun and a static box.
# Prints PASS/FAIL per check and exits non-zero on any FAIL.
set -u
B=${1:-$(command -v box)}; MODE=${2:-strict}
FAILS=0
export BOX_HOME=$(mktemp -d /tmp/bbfork-$MODE.XXXX)
OUT=$(mktemp -d /tmp/bbfork-outside.XXXX)
chmod 755 /tmp/bbfork-outside.* 2>/dev/null
ok(){ echo "  PASS $*"; }; bad(){ echo "  FAIL $*"; FAILS=$((FAILS + 1)); }
g(){ grep -v 'shared mount'; }
$B new p >/dev/null
printf 'base: docker.io/library/alpine:latest\ncpus: 1\nram_mib: 512\nisolation: %s\nwarm: 1\n' $MODE > $BOX_HOME/sandboxes/p/Boxfile
$B build p 2>&1 | grep -E 'isolated' | g
$B run p -- sh -c 'echo base > /data/keep; echo old > /data/change; echo x > /data/gone; mkdir /data/sub; echo s > /data/sub/a; ln -s '"$OUT"' /data/x; echo made' | g
S0=$(date +%s%N); $B fork p f1 | g; echo "  fork took $(( ($(date +%s%N)-S0)/1000000 ))ms"
$B fork p f2 >/dev/null
$B ls | g | grep -E 'f1|f2' | sed 's/^/  /'
$B run f1 -- sh -c 'echo new > /data/change; rm /data/gone; echo + > /data/added; rm -r /data/sub; mkdir /data/sub; echo b > /data/sub/b; rm /data/x; mkdir /data/x; echo pwned > /data/x/pwned; cat /data/keep' | g
$B run f2 -- sh -c 'echo f2 > /data/f2only' | g
echo "--- parent untouched while forks ran:"
[ "$($B run p -- sh -c 'cat /data/change; ls /data | tr "\n" " "' | g)" = "old
change gone keep sub x " ] && ok "parent /data unchanged" || bad "parent changed: $($B run p -- sh -c 'cat /data/change; ls /data' | g | tr '\n' ' ')"
[ -z "$(ls -A $OUT)" ] && ok "nothing escaped to the host via the planted symlink during the run" || bad "guest wrote outside: $(ls $OUT)"
echo "--- diff:"; $B diff f1 | sed 's/^/  /'
echo "--- guards:"
$B snapshot f1 2>&1 | sed 's/^/  /'; $B destroy p -y 2>&1 | sed 's/^/  /'; $B fork f1 f3 2>&1 | sed 's/^/  /'; $B rename p q 2>&1 | sed 's/^/  /'
echo "--- apply f1:"
$B apply f1 -y 2>&1 | g | sed 's/^/  /'
[ -z "$(ls -A $OUT)" ] && ok "apply did not follow the symlink out of /data" || bad "apply wrote outside: $(ls $OUT)"
got=$($B run p -- sh -c 'cat /data/change /data/added /data/sub/b 2>&1; ls /data/gone /data/sub/a 2>&1 | wc -l; [ -d /data/x ] && echo x-is-dir; cat /data/x/pwned' | g | tr '\n' ' ')
[ "$got" = "new + b 2 x-is-dir pwned " ] && ok "parent has the merged state" || bad "merged state wrong: $got"
[ -z "$($B diff f1)" ] && ok "f1 clean after apply" || bad "f1 still has changes"
[ "$($B run f1 -- cat /data/added | g)" = "+" ] && ok "f1 continues from merged state" || bad "f1 lost merged state"
echo "--- discard f2:"
$B discard f2 -y 2>&1 | g | sed 's/^/  /'
[ "$($B run f2 -- sh -c '[ -e /data/f2only ] && echo yes || echo no' | g)" = "no" ] && ok "f2 change discarded" || bad "f2 change survived discard"
[ "$($B run p -- sh -c 'ls /data/f2only 2>/dev/null | wc -l' | g)" = "0" ] && ok "discarded change never reached parent" || bad "f2 leaked"
echo "--- up/exec on a fork, then destroy:"
$B up f1 >/dev/null 2>&1 && $B exec f1 -- sh -c 'echo via-exec > /data/e' | g
$B apply f1 -y 2>&1 | sed 's/^/  /'   # refused while up
$B down f1 >/dev/null; $B apply f1 -y 2>&1 | sed 's/^/  /'
[ "$($B run p -- cat /data/e | g)" = "via-exec" ] && ok "exec on a fork applied to parent" || bad "exec change lost"
$B destroy f1 -y | g; $B destroy f2 -y | g; $B destroy p --data -y | g
[ ! -e $BOX_HOME/forks/f1 ] && ok "fork dirs removed" || bad "fork dir left"
pkill -f "^$B serve" 2>/dev/null; podman unshare rm -rf $BOX_HOME $OUT 2>/dev/null || rm -rf $BOX_HOME $OUT
exit $((FAILS > 0))
