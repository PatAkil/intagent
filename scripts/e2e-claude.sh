#!/usr/bin/env bash
# End-to-end test with two real Claude Code agents belonging to two members.
#
# Alice's agent reserves services/payments/** and starts a long task there.
# While it works, Bob's agent is told to edit the same file directly. intagent
# must refuse the edit with a reason, Bob's agent must adapt (leave the file
# alone, write a proposal, send Alice a note), and Alice's agent must receive
# the note at its next step.
#
# Needs: go, git, and an authenticated `claude` CLI. Costs a few cents of
# model usage. Usage: scripts/e2e-claude.sh [model]   (default: sonnet; KEEP=1 keeps
# the work directory after a pass)
set -euo pipefail

MODEL="${1:-sonnet}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
PORT="${PORT:-7477}"
URL="http://127.0.0.1:$PORT"
# On success the work directory, with the server's data and the agents' logs,
# goes unless KEEP=1; on failure it stays for a look.
finish() {
  local status=$?
  kill $(jobs -p) 2>/dev/null || true
  for _ in $(seq 50); do [ -z "$(jobs -pr)" ] && break; sleep 0.2; done
  kill -9 $(jobs -pr) 2>/dev/null || true
  pkill -f "intagent serve --config team.json --addr 127.0.0.1:$PORT" 2>/dev/null || true
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
  GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com

step() { printf '\n== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*"; exit 1; }

step "build"
mkdir -p "$WORK/bin"
(cd "$ROOT" && go build -o "$WORK/bin/intagent" ./cmd/intagent)
export PATH="$WORK/bin:$PATH"

step "team server"
cd "$WORK"
ALICE=$(intagent token add alice --config team.json | sed -n 3p | tr -d ' ')
BOB=$(intagent token add bob --config team.json | sed -n 3p | tr -d ' ')
# setsid: `claude -p` kills its process group on exit, which must not take the server with it.
setsid intagent serve --config team.json --addr "127.0.0.1:$PORT" --data "$WORK/data" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 50); do curl -fs "$URL/healthz" >/dev/null && break; sleep 0.1; done

step "repository with two clones"
git init -q --bare -b main origin.git
git clone -q origin.git alice 2>/dev/null
mkdir -p alice/services/payments
cat >alice/services/payments/retry.go <<'EOF'
package payments

// MaxRetries is how often a failed payment is retried.
const MaxRetries = 3
EOF
echo 'module example.com/payments' >alice/services/payments/go.mod
(cd alice && git add . && git commit -qm init && git push -q origin main)
as() { local who=$1; shift; INTAGENT_CONFIG="$WORK/$who.json" INTAGENT_HOST="$who-laptop" "$@"; }
(cd alice && as alice intagent login --url "$URL" --token "$ALICE" && as alice intagent init --agents claude-code >/dev/null \
  && git add -A && git commit -qm "enrol intagent" && git push -q origin main)
git clone -q origin.git bob
(cd bob && as bob intagent login --url "$URL" --token "$BOB")
(cd bob && as bob intagent doctor) || fail "doctor"

claude_as() {
  local who=$1 dir=$2 prompt=$3
  # --allowedTools: a folder Claude Code has not been told to trust ignores the
  # project's permissions.allow, so headless runs grant the intagent tools here.
  (cd "$dir" && as "$who" setsid claude -p "$prompt" --model "$MODEL" --permission-mode acceptEdits \
    --allowedTools 'mcp__intagent__*' \
    --session-id "$(python3 -c 'import uuid; print(uuid.uuid4())')" --max-turns 20 \
    --output-format stream-json --verbose </dev/null)
}

step "alice's agent reserves the payments package and works on it"
claude_as alice "$WORK/alice" "You are refactoring the payments retry logic. First, call the intagent declare_intent tool with mode \"exclusive\", paths [\"services/payments/**\"] and summary \"Rework payment retries\". Then change MaxRetries in services/payments/retry.go from 3 to 5. Then wait for the integration test run by running exactly this shell command in the foreground: until [ -f $WORK/done ]; do sleep 2; done; git status --short. Finally, reply with any notes from teammates that intagent showed you, quoting them." \
  >"$WORK/alice.jsonl" 2>"$WORK/alice.err" &
ALICE_PID=$!

for _ in $(seq 120); do
  # The server saves intents, compressed, in board.json.durable within a second or two.
  gzip -dcf "$WORK/data/board.json.durable" 2>/dev/null | grep -q '"pattern":"services/payments/\*\*"' &&
    grep -q 'until \[ -f' "$WORK/alice.jsonl" && break
  sleep 1
done
grep -q 'until \[ -f' "$WORK/alice.jsonl" || fail "alice's agent never reached its long-running step"

step "bob's agent is told to edit the same constant directly"
claude_as bob "$WORK/bob" "Your user asks you to change MaxRetries in services/payments/retry.go to 10 by editing the file directly with the Edit tool; try the edit first. If intagent refuses the edit, do not force it: write your proposed change to PROPOSAL.md instead, and use the intagent send_note tool to tell alice what you propose. Then reply with what you did." \
  >"$WORK/bob.jsonl" 2>"$WORK/bob.err" || true

step "alice's agent finishes and reads its news"
touch "$WORK/done"
wait "$ALICE_PID" || true

step "results"
show_board() { (cd "$WORK/bob" && as bob intagent board) || true; }
show_board
grep -q 'MaxRetries = 5' "$WORK/alice/services/payments/retry.go" || fail "alice's change is missing"
grep -q 'MaxRetries = 3' "$WORK/bob/services/payments/retry.go" || fail "bob's agent edited the reserved file"
grep -q "is part of a teammate's work" "$WORK/bob.jsonl" || fail "bob's agent was never refused"
[ -f "$WORK/bob/PROPOSAL.md" ] || fail "bob's agent did not write a proposal"
kill -0 "$SERVER_PID" 2>/dev/null || fail "the server died during the run"
grep -q 'mcp__intagent__send_note' "$WORK/bob.jsonl" || fail "bob's agent did not send a note"
grep -q 'Note from bob' "$WORK/alice.jsonl" || fail "alice's agent never received bob's note"
echo
echo "PASS: bob's agent was refused, wrote PROPOSAL.md, and alice's agent received the note."
