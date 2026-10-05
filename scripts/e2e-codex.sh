#!/usr/bin/env bash
# End-to-end test with the real Codex CLI, driven offline by a scripted model
# (scripts/fakemodel speaking the Responses API), through the wiring
# `intagent init --trust-codex` writes. Free and deterministic; needs go, git
# and npm (and jq).
#
# 1. Bob's Codex is told about the team when its session starts.
# 2. In code mode it edits a file Alice's active agent reserved, once with
#    apply_patch and once with a patch piped through the shell: both refused,
#    and the model reads the reason.
# 3. Carol trusts the hooks but has not installed intagent: her edit goes
#    through, because Codex fails open.
#
# Usage: scripts/e2e-codex.sh   (CODEX=/path/to/codex to skip the install,
# KEEP=1 to keep the work directory after a pass)
#
# Codex writes Carol's file in its sandbox, bubblewrap, which needs user
# namespaces. Ubuntu 24.04 keeps them from unprivileged users; there, run
# sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 first, as CI does.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
PORT="${PORT:-7480}"
LLM="${LLM_PORT:-18094}"
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
    [ "$status" -eq 0 ] || diagnose "$WORK"
    echo "workdir: $WORK"
  fi
}
trap finish EXIT
. "$ROOT/scripts/e2e-diagnose.sh"

export GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com \
  GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com XDG_CACHE_HOME="$WORK/cache"

step() { printf '\n== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*"; exit 1; }

step "build"
mkdir -p "$WORK/bin" "$WORK/model"
(cd "$ROOT" && go build -o "$WORK/bin/intagent" ./cmd/intagent && go build -o "$WORK/fakemodel" ./scripts/fakemodel)
if [ -z "${CODEX:-}" ]; then
  (cd "$WORK" && npm install --silent --no-audit --no-fund @openai/codex@0.160.0 >/dev/null)
  CODEX="$WORK/node_modules/.bin/codex"
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

step "repository: alice enrols it, and her agent reserves the payments package"
git init -q --bare -b main origin.git
git clone -q origin.git alice 2>/dev/null
mkdir -p alice/services/payments
printf 'package payments\n\nconst MaxRetries = 3\n' >alice/services/payments/retry.go
echo hi >alice/README.md
(cd alice && git add . && git commit -qm init && git push -q origin main)
as() { local who=$1; shift; INTAGENT_CONFIG="$WORK/$who.json" INTAGENT_HOST="$who-laptop" CODEX_HOME="$WORK/codex-$who" "$@"; }
(cd alice && as alice intagent login --url "$URL" --token "$ALICE" >/dev/null && as alice intagent init >/dev/null \
  && git add -A && git commit -qm "enrol intagent" && git push -q origin main)
A="$WORK/alice"
alice_hook() { (cd "$A" && echo "$1" | as alice intagent hook >/dev/null); }
alice_hook '{"session_id":"a1","cwd":"'"$A"'","hook_event_name":"SessionStart","source":"startup"}'
alice_hook '{"session_id":"a1","cwd":"'"$A"'","hook_event_name":"UserPromptSubmit","prompt":"Rework payment retries"}'
(cd alice && as alice intagent declare -x -m "Rework payment retries" 'services/payments/**' >/dev/null)
for who in bob carol; do
  git clone -q origin.git "$who" 2>/dev/null
  mkdir -p "$WORK/codex-$who"
  (cd "$who" && as "$who" intagent init --trust-codex >/dev/null) || fail "trusting Codex hooks for $who"
done
(cd bob && as bob intagent login --url "$URL" --token "$BOB" >/dev/null)

# codex <member> <PATH> <prompt>: one headless Codex run in that member's clone.
codex() {
  rm -f "$WORK"/model/req-*.json
  (cd "$WORK/$1" && env PATH="$2" CODEX_HOME="$WORK/codex-$1" CODEX_API_KEY=dummy \
    INTAGENT_CONFIG="$WORK/$1.json" INTAGENT_HOST="$1-laptop" \
    timeout 120 "$CODEX" exec -s workspace-write -c analytics.enabled=false \
    -c "openai_base_url=\"http://127.0.0.1:$LLM/v1\"" "$3" \
    </dev/null >"$WORK/$1.out" 2>"$WORK/$1.err") || fail "codex exited $? for $1 (see $WORK/$1.err)"
}
told() { grep -qF "$1" "$WORK"/model/req-*.json; }
# exec_js <javascript>: a code-mode turn that reports a nested tool's result or error.
exec_js() { jq -n --arg js "try { text(await $1) } catch (e) { text(String(e)) }" '{tool: ["exec"], input: {exec: $js}}'; }
PATCH=$'*** Begin Patch\n*** Update File: services/payments/retry.go\n@@\n-const MaxRetries = 3\n+const MaxRetries = 10\n*** End Patch\n'

step "bob's Codex edits alice's reserved file, by apply_patch and through the shell"
jq -n --argjson a "$(exec_js "tools.apply_patch($(jq -n --arg p "$PATCH" '$p'))")" \
  --argjson b "$(exec_js "tools.exec_command({cmd: $(jq -n --arg c "apply_patch <<'EOF'"$'\n'"$PATCH"'EOF' '$c')})")" \
  '{turns: [$a, $b, {text: "done"}]}' >"$WORK/script.json"
codex bob "$WORK/bin:$WITHOUT" "Raise MaxRetries to 10"
told "You are connected to your team's intagent board as bob's agent" || fail "bob's model was not told about the team"
[ "$(grep -ohF "Command blocked by PreToolUse hook: [intagent] services/payments/retry.go is part of a teammate's work" \
  "$WORK"/model/req-*.json | sort -u | wc -l)" -ge 1 ] || fail "bob's model did not read intagent's refusal"
last=$(ls "$WORK"/model/req-*.json | tail -1)
[ "$(grep -oF "Command blocked by PreToolUse hook" "$last" | wc -l)" -eq 2 ] || fail "both edits were not refused"
grep -q "MaxRetries = 3" bob/services/payments/retry.go || fail "the reserved file was changed"

step "carol trusts the hooks but has no intagent; her Codex edits a file"
P2=$'*** Begin Patch\n*** Update File: README.md\n@@\n-hi\n+hello\n*** End Patch\n'
jq -n --argjson a "$(exec_js "tools.apply_patch($(jq -n --arg p "$P2" '$p'))")" '{turns: [$a, {text: "done"}]}' >"$WORK/script.json"
PATH="$WITHOUT" command -v intagent >/dev/null && fail "intagent is installed outside the test"
codex carol "$WITHOUT" "Say hello in the README"
! told "Command blocked by PreToolUse hook" || fail "carol's edit was refused"
grep -qx hello carol/README.md || fail "carol's edit did not happen (can Codex's sandbox create user namespaces?)"

printf '\nPASS: bob was told about the team and refused twice, carol was left alone.\n'
