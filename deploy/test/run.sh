#!/usr/bin/env bash
# Tests deploy-streamstatus against fake docker, docker-compose and cosign.
# Usage: deploy/test/run.sh   (needs bash, flock, sha256sum)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=$here/../deploy-streamstatus
fails=0

D1=sha256:$(printf '1%.0s' {1..64})
D2=sha256:$(printf '2%.0s' {1..64})
D3=sha256:$(printf '3%.0s' {1..64})
REPO=ghcr.io/infosecstreams/streamstatus

setup() {
  T=$(mktemp -d)
  mkdir -p "$T/bin" "$T/hosting" "$T/fake/tags"
  cp "$here/fake-docker" "$T/bin/docker"
  cp "$here/fake-compose" "$T/bin/docker-compose"
  cp "$here/fake-cosign" "$T/bin/cosign"
  chmod +x "$T/bin/"*
  export FAKE="$T/fake" PATH="$T/bin:$PATH"
  export DEPLOY_HOSTING_DIR="$T/hosting" DEPLOY_COSIGN="$T/bin/cosign" DEPLOY_COMPOSE=docker-compose
  export DEPLOY_READY_TIMEOUT=3 DEPLOY_STABLE_SECONDS=0
  unset SSH_CONNECTION SSH_ORIGINAL_COMMAND DEPLOY_HEALTH_URL
  printf 'OTHER=keep-me\n' > "$T/hosting/.env"
}

# running sets up an existing container running the image pinned as $1.
running() {
  printf 'STREAMSTATUS_IMAGE=%s\nOTHER=keep-me\n' "$1" > "$T/hosting/.env"
  (cd "$T/hosting" && docker-compose up -d --no-deps streamstatus > /dev/null)
}

pin() { sed -n 's/^STREAMSTATUS_IMAGE=//p' "$T/hosting/.env"; }

check() { # check <name> <condition...>
  local name=$1; shift
  if "$@"; then echo "ok   $name"; else echo "FAIL $name"; fails=$((fails + 1)); fi
}

run() { "$script" "$@" > "$T/out" 2>&1; }

# --- a good deploy pins the digest and keeps other .env lines ---------------
setup; running "$REPO@$D1"
run deploy "$D2" && rc=0 || rc=$?
check "deploy succeeds" test "$rc" = 0
check "deploy pins new digest" test "$(pin)" = "$REPO@$D2"
check "deploy keeps other .env lines" grep -qx 'OTHER=keep-me' "$T/hosting/.env"
check "deploy tags previous image" test -f "$FAKE/tags/streamstatus-rollback:previous"
check "deploy writes history" grep -q "deploy $REPO@$D2" "$T/hosting/.deploy-streamstatus/history"
run deploy "$D2" && rc=0 || rc=$?
check "redeploy of same digest is a no-op" grep -q "nothing to do" "$T/out"

# --- an unsigned image changes nothing -------------------------------------
setup; running "$REPO@$D1"; echo "$REPO@$D2" > "$FAKE/unsigned"
before=$(cat "$FAKE/container")
run deploy "$D2" && rc=0 || rc=$?
check "unsigned image is rejected" test "$rc" != 0
check "unsigned image leaves pin alone" test "$(pin)" = "$REPO@$D1"
check "unsigned image is never run" test "$(cat "$FAKE/container")" = "$before"
check "unsigned image is never pulled" test ! -e "$FAKE/pulled"

# --- verify mode pulls and checks but changes nothing ----------------------
setup; running "$REPO@$D1"; before=$(cat "$FAKE/container")
run verify "$D2" && rc=0 || rc=$?
check "verify succeeds" test "$rc" = 0
check "verify pulls" grep -qx "$REPO@$D2" "$FAKE/pulled"
check "verify leaves pin and container alone" test "$(pin)" = "$REPO@$D1" -a "$(cat "$FAKE/container")" = "$before"

# --- a bot that never gets ready is rolled back ----------------------------
setup; running "$REPO@$D1"; echo "$REPO@$D3" > "$FAKE/never_ready"
run deploy "$D3" && rc=0 || rc=$?
check "unready deploy fails" test "$rc" != 0
check "unready deploy rolls back pin" test "$(pin)" = "$REPO@$D1"
check "unready deploy runs old image again" test "$(cat "$FAKE/running_ref")" = "$REPO@$D1"
check "unready deploy logs rollback" grep -q "rollback to $REPO@$D1 is healthy" "$T/out"

# --- a crash-looping bot is rolled back ------------------------------------
setup; running "$REPO@$D1"; echo "$REPO@$D3" > "$FAKE/crashloop"
run deploy "$D3" && rc=0 || rc=$?
check "crash-looping deploy fails" test "$rc" != 0
check "crash-looping deploy rolls back pin" test "$(pin)" = "$REPO@$D1"

# --- first ever deploy from an unpinned :latest rolls back to the old image -
setup; running "$REPO:latest"; echo "$REPO@$D3" > "$FAKE/never_ready"
run deploy "$D3" && rc=0 || rc=$?
check "unpinned rollback uses the tagged previous image" test "$(pin)" = "streamstatus-rollback:previous"
check "unpinned rollback runs the old image" test "$(cat "$FAKE/running_ref")" = "streamstatus-rollback:previous"

# --- manual rollback after a good deploy -----------------------------------
setup; running "$REPO@$D1"
run deploy "$D2" > /dev/null
run rollback && rc=0 || rc=$?
check "manual rollback succeeds" test "$rc" = 0
check "manual rollback restores previous pin" test "$(pin)" = "$REPO@$D1"

# --- SSH requests: only "<mode> <digest>" gets through ---------------------
setup
for bad in "" "deploy" "deploy sha256:abc" "deploy $D1; id" "deploy $D1 extra" "rollback" "status" \
           "verify  $D1" "DEPLOY $D1" "deploy ${D1}0" "deploy $D1"$'\n'"id" "bash -i"; do
  rc=0; SSH_CONNECTION="203.0.113.1 1 192.0.2.1 22" SSH_ORIGINAL_COMMAND="$bad" "$script" > "$T/out" 2>&1 || rc=$?
  check "ssh rejects $(printf %q "$bad")" test "$rc" = 2
done
rc=0; SSH_CONNECTION="203.0.113.1 1 192.0.2.1 22" "$script" > "$T/out" 2>&1 || rc=$?
check "ssh without a command is rejected" test "$rc" = 2
check "ssh rejections never touched docker" test ! -e "$FAKE/calls"
# Over SSH the environment can't redirect the script elsewhere.
rc=0; SSH_CONNECTION="203.0.113.1 1 192.0.2.1 22" SSH_ORIGINAL_COMMAND="verify $D1" \
  DEPLOY_HOSTING_DIR="$T/hosting" "$script" > "$T/out" 2>&1 || rc=$?
check "ssh ignores DEPLOY_* overrides" grep -q "no /home/ec2-user/hosting" "$T/out"

echo
if ((fails)); then echo "$fails test(s) failed"; exit 1; fi
echo "all deploy-streamstatus tests passed"
