#!/usr/bin/env bash
# End-to-end test with the real Gemini CLI, driven offline by a scripted model
# (scripts/fakemodel), through the wiring `intagent init` commits. Free and
# deterministic; needs go, git and npm.
#
# 1. Bob's Gemini is told about the team when its session starts.
# 2. It writes a file Alice reserved: refused, and the model reads the reason.
# 3. It writes a file in the package Alice's agent is changing: allowed, and
#    the heads-up reaches the model with the write's result (Gemini drops
#    context given before a tool runs, so intagent holds it until after).
# 4. Carol has not installed intagent: her Gemini edits as usual.
#
# Usage: scripts/e2e-gemini.sh   (GEMINI=/path/to/gemini to skip the install,
# KEEP=1 to keep the work directory after a pass)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
PORT="${PORT:-7479}"
LLM="${LLM_PORT:-18093}"
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
mkdir -p "$WORK/bin" "$WORK/model" "$WORK/home-bob/.gemini" "$WORK/home-carol/.gemini"
(cd "$ROOT" && go build -o "$WORK/bin/intagent" ./cmd/intagent && go build -o "$WORK/fakemodel" ./scripts/fakemodel)
if [ -z "${GEMINI:-}" ]; then
  (cd "$WORK" && npm install --silent --no-audit --no-fund @google/gemini-cli@0.62.0 >/dev/null)
  GEMINI="$WORK/node_modules/.bin/gemini"
fi
# With only GOOGLE_GEMINI_BASE_URL, Gemini CLI picks an auth type that headless runs reject.
for who in bob carol; do
  echo '{"security":{"auth":{"selectedType":"gemini-api-key"}}}' >"$WORK/home-$who/.gemini/settings.json"
done
WITHOUT="$PATH"
export PATH="$WORK/bin:$PATH"

step "team server and model"
cd "$WORK"
ALICE=$(intagent token add alice --config team.json | sed -n 3p | tr -d ' ')
BOB=$(intagent token add bob --config team.json | sed -n 3p | tr -d ' ')
intagent serve --config team.json --addr "127.0.0.1:$PORT" --data "$WORK/data" >"$WORK/server.log" 2>&1 &
"$WORK/fakemodel" -addr "127.0.0.1:$LLM" -script "$WORK/script.json" -log "$WORK/model" 2>"$WORK/model.log" &
for _ in $(seq 50); do curl -fs "$URL/healthz" >/dev/null && curl -fs "http://127.0.0.1:$LLM/v1/models" >/dev/null && break; sleep 0.1; done

step "repository: alice enrols it, reserves payments and works on billing"
git init -q --bare -b main origin.git
git clone -q origin.git alice 2>/dev/null
mkdir -p alice/services/payments alice/services/billing
printf 'package payments\n\nconst MaxRetries = 3\n' >alice/services/payments/retry.go
printf 'package billing\n' >alice/services/billing/invoice.go
echo 'module example.com/payments' >alice/services/payments/go.mod
echo 'module example.com/billing' >alice/services/billing/go.mod
echo hi >alice/README.md
(cd alice && git add . && git commit -qm init && git push -q origin main)
as() { local who=$1; shift; INTAGENT_CONFIG="$WORK/$who.json" INTAGENT_HOST="$who-laptop" "$@"; }
(cd alice && as alice intagent login --url "$URL" --token "$ALICE" >/dev/null && as alice intagent init >/dev/null \
  && git add -A && git commit -qm "enrol intagent" && git push -q origin main)
(cd alice && as alice intagent declare -x -m "Rework payment retries" 'services/payments/**' >/dev/null)
# Alice's Claude Code edits invoice.go, through the hook it would run.
A="$WORK/alice"
alice_hook() { (cd "$A" && echo "$1" | as alice intagent hook >/dev/null); }
alice_hook '{"session_id":"a1","cwd":"'"$A"'","hook_event_name":"SessionStart","source":"startup"}'
alice_hook '{"session_id":"a1","cwd":"'"$A"'","hook_event_name":"UserPromptSubmit","prompt":"Add tax lines to invoices"}'
alice_hook '{"session_id":"a1","cwd":"'"$A"'","hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"'"$A"'/services/billing/invoice.go"}}'
git clone -q origin.git bob 2>/dev/null
git clone -q origin.git carol 2>/dev/null
# Bob signs in as a member does, into the configuration in their home. Gemini
# CLI hands its hooks only PATH, HOME and a few more when it runs in GitHub
# Actions (GITHUB_SHA is set), so INTAGENT_CONFIG would not reach them there.
(cd bob && env -u INTAGENT_CONFIG -u XDG_CONFIG_HOME HOME="$WORK/home-bob" \
  intagent login --url "$URL" --token "$BOB" >/dev/null)

# gemini <member> <PATH> <prompt>: one headless Gemini CLI run in that member's
# clone, with the member's home.
gemini() {
  rm -f "$WORK"/model/req-*.json
  (cd "$WORK/$1" && env -u INTAGENT_CONFIG -u XDG_CONFIG_HOME PATH="$2" HOME="$WORK/home-$1" \
    GEMINI_CLI_TRUST_WORKSPACE=true GEMINI_API_KEY=fake GOOGLE_GEMINI_BASE_URL="http://127.0.0.1:$LLM" \
    timeout 120 "$GEMINI" -p "$3" --yolo -m gemini-2.5-flash </dev/null >"$WORK/$1.out" 2>"$WORK/$1.err") \
    || fail "gemini exited $? for $1 (see $WORK/$1.err)"
}
told() { grep -qF "$1" "$WORK"/model/req-*.json; }

step "bob's Gemini writes alice's reserved file, then a file next to her work"
B="$WORK/bob"
cat >"$WORK/script.json" <<EOF
{"turns": [
  {"tool": ["write_file"], "args": {"write_file": {"file_path": "$B/services/payments/retry.go", "content": "package payments\n\nconst MaxRetries = 10\n"}}},
  {"tool": ["write_file"], "args": {"write_file": {"file_path": "$B/services/billing/tax.go", "content": "package billing\n"}}},
  {"text": "done"}]}
EOF
gemini bob "$WORK/bin:$WITHOUT" "Raise MaxRetries to 10 and add a tax file"
told "You are connected to your team's intagent board as bob's agent" || fail "bob's model was not told about the team"
told "Tool execution blocked: [intagent] services/payments/retry.go is part of a teammate's work" \
  || fail "bob's model did not read intagent's refusal"
grep -q "MaxRetries = 3" bob/services/payments/retry.go || fail "the reserved file was changed"
[ -f bob/services/billing/tax.go ] || fail "the allowed write did not happen"
told "Heads-up" && told "same area services/billing" || fail "the heads-up did not reach bob's model after the write"

step "carol, without intagent, writes a file"
cat >"$WORK/script.json" <<EOF
{"turns": [{"tool": ["write_file"], "args": {"write_file": {"file_path": "$WORK/carol/README.md", "content": "hello\n"}}}, {"text": "done"}]}
EOF
PATH="$WITHOUT" command -v intagent >/dev/null && fail "intagent is installed outside the test"
gemini carol "$WITHOUT" "Say hello in the README"
! told "Tool execution blocked" || fail "carol's write was refused"
grep -qx hello carol/README.md || fail "carol's write did not happen"

printf '\nPASS: bob was told about the team, refused, and warned after the write; carol was left alone.\n'
