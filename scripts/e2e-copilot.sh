#!/usr/bin/env bash
# End-to-end test with the real GitHub Copilot CLI, driven offline by a
# scripted model (scripts/fakemodel), through the wiring `intagent init`
# commits. Free and deterministic; needs go, git and npm.
#
# 1. Bob's Copilot is told about the team when its session starts.
# 2. Bob's Copilot edits a file Alice reserved, with a relative path as models
#    write them: the edit is refused and the model reads intagent's reason.
# 3. With a codex model Copilot edits only through apply_patch: refused too.
# 4. Carol has not installed intagent: her Copilot edits as if intagent were
#    not there, although the hooks are committed.
#
# Usage: scripts/e2e-copilot.sh   (COPILOT=/path/to/copilot to skip the install,
# KEEP=1 to keep the work directory after a pass)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
PORT="${PORT:-7478}"
LLM="${LLM_PORT:-18092}"
URL="http://127.0.0.1:$PORT"
# On success the work directory, hundreds of MB with the agent's npm install,
# goes unless KEEP=1; on failure it stays for a look.
finish() {
  local status=$?
  kill $(jobs -p) 2>/dev/null || true
  for _ in $(seq 50); do [ -z "$(jobs -pr)" ] && break; sleep 0.2; done
  kill -9 $(jobs -pr) 2>/dev/null || true
  if [ "$status" -eq 0 ] && [ "${KEEP:-}" != 1 ]; then
    rm -rf "$WORK" 2>/dev/null || echo "workdir: $WORK (not all of it could be removed)"
  else
    echo "workdir: $WORK"
  fi
}
trap finish EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com \
  GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com XDG_CACHE_HOME="$WORK/cache"

step() { printf '\n== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*"; exit 1; }

step "build"
mkdir -p "$WORK/bin" "$WORK/model" "$WORK/home"
(cd "$ROOT" && go build -o "$WORK/bin/intagent" ./cmd/intagent && go build -o "$WORK/fakemodel" ./scripts/fakemodel)
if [ -z "${COPILOT:-}" ]; then
  (cd "$WORK" && npm install --silent --no-audit --no-fund @github/copilot@1.0.91 >/dev/null)
  COPILOT="$WORK/node_modules/.bin/copilot"
fi
WITHOUT="$PATH"
export PATH="$WORK/bin:$PATH"

step "team server and model"
cd "$WORK"
ALICE=$(intagent token add alice --config team.json | sed -n 3p | tr -d ' ')
BOB=$(intagent token add bob --config team.json | sed -n 3p | tr -d ' ')
intagent serve --config team.json --addr "127.0.0.1:$PORT" --data "$WORK/data" >"$WORK/server.log" 2>&1 &
"$WORK/fakemodel" -addr "127.0.0.1:$LLM" -script "$WORK/script.json" -log "$WORK/model" 2>"$WORK/model.log" &
for _ in $(seq 50); do curl -fs "$URL/healthz" >/dev/null && curl -fs "http://127.0.0.1:$LLM/v1/models" >/dev/null && break; sleep 0.1; done

step "repository: alice enrols it and reserves the payments package"
git init -q --bare -b main origin.git
git clone -q origin.git alice 2>/dev/null
mkdir -p alice/services/payments
printf 'package payments\n\nconst MaxRetries = 3\n' >alice/services/payments/retry.go
echo hi >alice/README.md
(cd alice && git add . && git commit -qm init && git push -q origin main)
as() { local who=$1; shift; INTAGENT_CONFIG="$WORK/$who.json" INTAGENT_HOST="$who-laptop" "$@"; }
(cd alice && as alice intagent login --url "$URL" --token "$ALICE" >/dev/null && as alice intagent init >/dev/null \
  && git add -A && git commit -qm "enrol intagent" && git push -q origin main)
(cd alice && as alice intagent declare -x -m "Rework payment retries" 'services/payments/**' >/dev/null)
git clone -q origin.git bob 2>/dev/null
git clone -q origin.git carol 2>/dev/null
(cd bob && as bob intagent login --url "$URL" --token "$BOB" >/dev/null)

# copilot <member> <PATH> <prompt> [model]: one headless Copilot run in that member's clone.
copilot() {
  rm -f "$WORK"/model/req-*.json
  (cd "$WORK/$1" && env PATH="$2" HOME="$WORK/home" COPILOT_HOME="$WORK/home" COPILOT_OFFLINE=true \
    COPILOT_PROVIDER_BASE_URL="http://127.0.0.1:$LLM/v1" COPILOT_MODEL="${4:-fake-model}" COPILOT_ALLOW_ALL=true \
    INTAGENT_CONFIG="$WORK/$1.json" INTAGENT_HOST="$1-laptop" \
    timeout 120 "$COPILOT" -p "$3" >"$WORK/$1.out" 2>"$WORK/$1.err") || fail "copilot exited $? for $1"
}
told() { grep -qF "$1" "$WORK"/model/req-*.json; }

step "bob's Copilot edits alice's reserved file"
cat >"$WORK/script.json" <<'EOF'
{"turns": [{"tool": ["edit"], "args": {"edit": {"path": "services/payments/retry.go", "old_str": "MaxRetries = 3", "new_str": "MaxRetries = 10"}}}, {"text": "done"}]}
EOF
copilot bob "$WORK/bin:$WITHOUT" "Raise MaxRetries to 10"
told "You are connected to your team's intagent board as bob's agent" || fail "bob's model was not told about the team"
told "Denied by preToolUse hook: [intagent] services/payments/retry.go is part of a teammate's work" \
  || fail "bob's model did not read intagent's refusal"
grep -q "MaxRetries = 3" bob/services/payments/retry.go || fail "the reserved file was changed"
(cd bob && as bob intagent board) | grep -q "1 collision caught before the edit" || fail "the board did not count the refusal"

step "bob's Copilot, on a codex model, patches the reserved file with apply_patch"
jq -n --arg p $'*** Begin Patch\n*** Update File: services/payments/retry.go\n@@\n-const MaxRetries = 3\n+const MaxRetries = 10\n*** End Patch\n' \
  '{turns: [{tool: ["apply_patch"], input: {apply_patch: $p}}, {text: "done"}]}' >"$WORK/script.json"
copilot bob "$WORK/bin:$WITHOUT" "Raise MaxRetries to 10" gpt-5-codex
told '"apply_patch"' || fail "Copilot did not offer apply_patch to a codex model"
told "Denied by preToolUse hook: [intagent] services/payments/retry.go is part of a teammate's work" \
  || fail "the apply_patch edit was not refused"
grep -q "MaxRetries = 3" bob/services/payments/retry.go || fail "apply_patch changed the reserved file"

step "carol, without intagent, edits a file"
cat >"$WORK/script.json" <<'EOF'
{"turns": [{"tool": ["edit"], "args": {"edit": {"path": "README.md", "old_str": "hi", "new_str": "hello"}}}, {"text": "done"}]}
EOF
command -v intagent >/dev/null && PATH="$WITHOUT" command -v intagent >/dev/null && fail "intagent is installed outside the test"
copilot carol "$WITHOUT" "Say hello in the README"
! told "Denied by preToolUse hook" || fail "carol's edit was refused"
grep -qx hello carol/README.md || fail "carol's edit did not happen"

printf '\nPASS: bob was told about the team and refused, carol was left alone.\n'
