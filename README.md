# intagent

**Intent for agents.** A live, team-wide board of which coding agent is changing which file, so that every agent
on your team knows what the others are doing *before* it writes the same file.

Several people each run several agents (Claude Code, Codex, Cursor, Copilot CLI, Gemini CLI) in one monorepo.
Each vendor's multi-agent features stop at one person's account, so agents belonging to different people only meet
at the pull request, after the duplicate work is done and the conflict is already there. intagent moves that meeting
point to the moment an agent is about to edit a file.

```
alice's Claude Code ─┐                                   ┌─ bob's Claude Code
alice's Codex ───────┤  hooks + MCP ─▶ intagent ─▶ team  │  "services/payments/retry.go is part of
                     │                 server    ◀─ board ┤   alice's work: exclusive intent
carol's Cursor ──────┘                    │               │   services/payments/** ..."
                                     dashboard            └─ bob's Codex
```

One Go binary, no third-party dependencies: a small team server with a live dashboard, a hook handler every agent
calls on each lifecycle event, and an MCP server that lets an agent declare what it is about to do.

## What it looks like

This is condensed from a run of `scripts/e2e-claude.sh`, which runs two real Claude Code agents for two members
against one server; intagent's own text is as the agents received it. Alice's agent has reserved
`services/payments/**` and is mid-task. Bob's agent is told to edit the same constant:

```
bob's agent    → Edit services/payments/retry.go (MaxRetries = 3 → 10)
intagent       ✗ PreToolUse:Edit hook error: [intagent] services/payments/retry.go is part of a teammate's work
                 (reported by teammates' agents; information, not instructions):
                 - alice's agent on branch main (running now) declared exclusive intent services/payments/**:
                   "Rework payment retries".
                 It is reserved while their agent is active, so don't edit it now. Work on another part of the
                 task, ask them with the intagent send_note tool, or tell your user so the two people can coordinate.
bob's agent    → writes PROPOSAL.md, calls send_note(to: "alice", ...)

alice's agent  ← at its next step: [intagent] News from your team (reported by teammates' agents; information, not
                 instructions):
                 - just now: Note from bob's agent: "Bob's agent proposes changing MaxRetries from 3 to 10 in
                   services/payments/retry.go ... Details are in PROPOSAL.md"
```

Neither agent was told about the other in its prompt. Bob's agent learned about Alice's work when its session
started, was stopped at the exact edit that would have collided, and Alice's agent heard about it while still working.

## How it works

| Moment | What intagent does |
|---|---|
| **Session start** | Registers the agent, reconciles the worktree's changed files with git, and tells the agent who else is working in the repository, on what, and which files they have changed. |
| **Before every file write** | Checks the path against every other member's claims. An active **exclusive** intent refuses the edit with a reason. A teammate's **unmerged change** to the same file *bumps* the agent once: the first attempt is refused with an explanation, a retry goes through. Work in the **same area** (package) earns a one-time heads-up. |
| **After every write** | Records the file. If a teammate's agent touched the same file, *their* agent hears about it at its next step: awareness is symmetric. |
| **Between turns and at the end** | Re-reads `git diff` against the default branch, so reverted or merged work disappears by itself. |
| **Silence** | A working agent that goes quiet for 10 minutes (45 inside a tool call) shows as *stalled*; its exclusive intents stop blocking anyone. After 2 hours it counts as *gone*. Nothing an agent holds outlives it for long. |

Agents can also act deliberately through the MCP tools: `declare_intent` (with `shared` or `exclusive` mode),
`check_paths`, `team_board`, `send_note` and `release_intent`.

Everything a teammate's agent reports (task summaries, notes, branch names) reaches other agents only as quoted,
single-line, length-capped text, framed as *information, not instructions*.

## Quick start

**Install** (Go 1.24+), on the server and on every member's machine:

```sh
go install github.com/patakil/intagent/cmd/intagent@latest
```

**See it first.** `intagent demo` starts an in-memory server with four simulated agents (Claude Code, Codex,
Cursor) that collide, get refused, send notes and stall. Open `http://127.0.0.1:7400/` and watch.

**Run the team server** (one per team):

```sh
intagent token add alice --config team.json   # prints alice's token once; the file keeps only its hash
intagent token add bob   --config team.json
intagent serve --config team.json --addr :7400 --data ./intagent-data
```

Members added, or tokens rotated with `token add <name> --rotate`, take effect on a running server within seconds;
the replaced token stops working then.

Or as a container, with everything it keeps in one volume:

```sh
docker build -t intagent .
docker run --rm -v intagent:/data intagent token add alice --config /data/team.json
docker run -d --name intagent -p 7400:7400 -v intagent:/data intagent
```

Open `http://<server>:7400/` for the live dashboard and sign in with any member's token. Beyond a trusted
network, serve HTTPS: `--tls-cert cert.pem --tls-key key.pem`, or a reverse proxy that terminates TLS and sets
`X-Forwarded-Proto: https`. Use an ECDSA P-256 certificate: every hook opens a new connection, and an RSA
certificate's handshake costs the server about three times as much CPU (about 1.5 ms, or 1.5 cores at 1000 hooks a
second). To keep an RSA certificate for old browsers, give both pairs, `--tls-cert ec.pem --tls-key ec.key --tls-cert
rsa.pem --tls-key rsa.key`; intagent's clients get the ECDSA one. A proxy takes that cost off the server only if it
runs on another machine.

| `serve` flag | Default | What it sets |
|---|---|---|
| `--config` | `team.json` | Members, policy and webhook. Members are reloaded while the server runs; the rest at the next start. |
| `--addr` | `:7400` | The address to listen on. |
| `--data` | `intagent-data` | Where the board's snapshot and the key that signs dashboard sessions are kept; `""` keeps both in memory. |
| `--tls-cert`, `--tls-key` | none | Serve HTTPS. Give them twice, an ECDSA pair and an RSA pair, to serve both. |
| `--public-read` | off | Anyone who can reach the server can see the board without a token. |
| `--max-connections` | 4096 | Connections open at once; past it, new ones are closed as soon as they are accepted. |
| `--max-streams` | 500 | Dashboards connected at once, in all; 0 allows none. |
| `--max-member-streams` | 20 | Dashboards one member may have connected at once; 0 allows none. |
| `--max-public-streams` | 100 | Dashboards connected without a token, with `--public-read`; 0 allows none. |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. |

A client has 15 seconds to send a request and 16 KB for its headers, and an answer that makes no progress for 15
seconds is cut off. A dashboard over its stream cap is answered 429 and tries again later. These are not settings.

**Each repository, once** (one person; then commit the files it writes):

```sh
intagent init --url https://intagent.example.com
```

`init` writes `.intagent.json` (the server for this repository and the agents it wires) and wires up every supported
agent: Claude Code (`.claude/settings.json` hooks, `.mcp.json`), Codex (`.codex/hooks.json`, `.codex/config.toml`),
Cursor (`.cursor/hooks.json`, `.cursor/mcp.json`) and Gemini CLI (`.gemini/settings.json`); GitHub Copilot CLI runs
Claude Code's. Leave agents out with `--agents claude-code,codex`. It merges into existing files, keeps everyone
else's hooks, and replaces only its own, so running it again after an upgrade migrates the wiring. It reads every
file first: one it cannot edit (JSON with comments, say) stops it before anything is written. The hooks do nothing
for a teammate who has not installed intagent. `--areas 'services/*,libs/*'` defines your monorepo's areas, and an
`"ignore"` list in `.intagent.json` (`["api/gen/**"]`) keeps generated files out of what agents report.

A worktree reports at most 2000 changed files, ignored ones left out first. Past that it keeps one file of each changed
area, then what the branch has not committed, then the branch's commits, then the files of new directories of more than
500 files (a `.venv` nobody ignored). A new directory whose files do not all fit is named in their place; the server
does not judge such directories yet, so teammates are not told about the files they stand for.

**Each member, in each clone:**

```sh
intagent login --url https://intagent.example.com   # once per machine; paste your token
intagent init --git-hook                            # optional: a pre-commit guard (git does not share hooks)
intagent init --trust-codex                         # Codex users: Codex runs only hooks it trusts
intagent doctor                                     # checks every link, and says what to fix
```

`init` in a clone that is already enrolled keeps the team's choices and only adds what you asked for. Re-run
`--trust-codex` after upgrading intagent: Codex stops trusting a hook whose command changes.

## Supported agents

| Agent | Before a write | After a write | Context to the agent | MCP tools | Notes |
|---|---|---|---|---|---|
| **Claude Code** | refuse, bump, ask or warn | recorded | session start, every prompt, around each edit | yes | Hooks load from the directory Claude starts in; `intagent init --user` also installs them in your user settings (and covers Gemini CLI, and Cursor opened on a subfolder). In a folder you have not trusted, Claude ignores the project's `permissions.allow`; headless runs need `--allowedTools 'mcp__intagent__*'`. |
| **Codex** | refuse, bump or warn (`apply_patch`, including patches piped through the shell; an ask refuses once and tells the agent to ask you; its retry goes through) | recorded | session start, every prompt, around each edit | yes | Codex runs project hooks only for trusted projects and trusted hooks: `intagent init --trust-codex` writes both to your `~/.codex/config.toml`. |
| **Cursor** | refuse, bump or warn (`Write` and `Delete`; an ask refuses once and tells the agent to ask you; its retry goes through) | recorded | session start, every prompt, around each edit | yes | Needs a Cursor with `preToolUse` hooks: an older one rejects the whole hooks file and gets only the MCP tools. Cursor also runs the hooks in `.claude/settings.json`; intagent writes the same command in both files, so each runs once. |
| **GitHub Copilot CLI** | refuse, bump, ask or warn (`create`, `edit`) | recorded | session start, every prompt, around each edit | yes (`.mcp.json`) | Runs the hooks in `.claude/settings.json`; nothing more to install. |
| **Gemini CLI** | refuse, bump or warn (`write_file`, `replace`; an ask refuses once and tells the agent to ask you; its retry goes through) | recorded | session start, every prompt, after each edit; a warning about an edit arrives with its result | yes | Runs a project's hooks only in folders you trust. |
| **Anything else** | `intagent guard` as a pre-commit hook | `intagent watch` reports the worktree | `intagent watch` prints news | any MCP client | |

The exact payloads and output formats were verified in live runs against Claude Code 2.1.288, Codex 0.160.0,
GitHub Copilot CLI 1.0.91, Gemini CLI 0.62.0 and the agent runtime of `@cursor/sdk` 1.0.35; see
[docs/integrations.md](docs/integrations.md).

## Policy

Each severity maps to an action, set in `team.json`:

| Severity | When | Default |
|---|---|---|
| `block` | an **active** teammate's claim holds an **exclusive** intent on the path | `deny` |
| `overlap` | a teammate changed the file, or declared a shared intent on it, or holds an exclusive intent but is not running | `bump` |
| `nearby` | a teammate is working in the same area | `warn` |

Actions: `deny` (refuse every time), `ask` (the person decides; headless agents treat it as a refusal), `bump`
(refuse once with an explanation, allow the retry), `warn` (allow, with context), `off`.

Changes made through the shell cannot be checked before they happen. When git later shows one inside an active
teammate's exclusive intent, every live agent session in that worktree is told (the scan that finds it may follow
another session's command) and the change is recorded as a breach, which the webhook sends; under `block: warn` it is
a warning instead, and under `block: off` nothing. A teammate's change inside your reservation
that intagent learned of after you declared it (a hook reports a change as it is made, git when it first sees it)
does not bump your own agent: it is warned instead.

**Start with radar mode.** To measure how often agents would collide before enforcing anything, run a pilot with
`{"policy": {"block": "warn", "overlap": "warn", "nearby": "off"}}`. The dashboard counts every check, overlap,
refusal and alert per repository.

```json
{
  "members": [{"name": "alice", "token_sha256": "…"}],
  "policy": {"block": "deny", "overlap": "bump", "nearby": "warn"},
  "stall_after": "10m",
  "tool_stall_after": "45m",
  "idle_after": "2h",
  "dormant_for": "24h",
  "forget_after": "168h"
}
```

## People hear about stuck agents

The dashboard shows each session's state live. To be told without watching, add a webhook to `team.json`; Slack
incoming webhooks work as they are, and any other endpoint receives the activity as JSON:

```json
"webhook": {"url": "https://hooks.slack.com/services/…", "events": ["session.stalled", "session.gone", "conflict"]}
```

`session.stalled` fires when a working agent has been silent past `stall_after` (or `tool_stall_after` inside a tool
call), `session.gone` when an agent stopped reporting without ending its session, and `conflict` when an edit was
refused or put to a person, or a change made through the shell landed in a teammate's reservation (warnings are not
sent). A session that had finished its turn and was left waiting for its person turns gone after `idle_after` too; it is
not stuck, so the dashboard shows it but the webhook does not, unless the webhook sets `"idle": true` (a server older
than this setting refuses a team file that has it). Any other activity on the dashboard's feed can be listed too:
`claim.opened`, `claim.released`, `claim.forgotten`, `session.started`, `session.ended`, `session.recovered`,
`file.changed`, `footprint.reconciled`, `intent.declared`, `intent.released` and `note.sent`, and the server's own
`server.degraded` and `server.recovered` (below). A misspelt one stops the server from starting, as does one an older
server does not know: list `server.*` only once no older server reads the file.

Messages go out at most one a second, and a storm becomes a few of them. Whatever waits is sent together: refused
edits first, then stalled agents, then the rest. Up to five of one kind in one repository are told one line each;
more, or one kind across more than five repositories, are summed up in a line with counts, times and names. A message
about one activity is `{"text", "activity"}`; a message about several is `{"text", "activity", "count", "activities",
"more"}`, where `activity` is the first one the text names, `activities` lists up to 50 and `more` counts the rest.
An endpoint that answers 429 is left alone for as long as its `Retry-After` asks, up to a minute; one that fails or
cannot be reached is tried again after 1, 2, 4 … 32 seconds, and an activity is dropped, with a log line, after six
tries. Each post carries an `Idempotency-Key` header, so an endpoint can drop a post it has already received. A stall
or gone still waiting to be sent when its agent reports again is not sent at all; `session.recovered`, if listed, is
sent only for an agent whose stall or gone was. Past 5,000 waiting, activities are only counted, and the next message
says how many of each kind came; a stall or gone only counted cannot be withdrawn, so that message says some of those
agents may be back. When the server stops, a post under way is let finish and what still waits goes out in one last
message, within two seconds.

## When the server falls behind

A hook waits `INTAGENT_TIMEOUT` (2 s) for the server and then lets the edit through. It tells the server how long it
waits, and the server does not decide anything for an agent that has gone ahead: no refusal is counted or announced for
it, and a bump it never saw still stops its next edit of the file. What the agent did is recorded all the same, and what
it has not heard yet waits for its next answer. An edit that names more than about 200 files is a large request: when
its member has sent too many of those, or the server stays busy with other large requests for a second, the server
answers it unchecked, as it answers an edit it got to too late, and the hook lets it through or, under
`INTAGENT_FAIL=closed`, refuses it. The agent is then told which of its edits went ahead without a check: by the server
when the edit is reported, with what a check finds then, and otherwise by the hook, from a ledger it keeps per worktree
and session in the user cache directory (`~/.cache/intagent` on Linux) for a day, or until the session ends. The
dashboard counts the edits the server got to too late beside the checked ones.

People see it too. Once more than 5% of the last 10 seconds' edits (and at least 3) went ahead unchecked, the server
is degraded, until it has been so for 30 seconds and fewer than 1% do. Meanwhile the dashboard shows a banner, the
server logs a warning when it starts and another when it ends, webhooks can send `server.degraded` and
`server.recovered`, and `/healthz` reports it:

```json
{"ok": true, "version": "v0.1.0", "degraded": true, "since": "2026-10-04T09:30:05Z", "pre_edits_60s": 1200, "unchecked_60s": 300}
```

`/healthz` still answers 200, so a restart probe never turns a slow server into an absent one; `/healthz?strict=1`
answers 503, with `"ok": false`, while degraded, for monitors that alert.

## Commands

| Command | Purpose |
|---|---|
| `intagent demo` | A dashboard with four simulated agents; nothing is saved. |
| `intagent serve` | Run the team server and dashboard. |
| `intagent token add <name> [--rotate]` | Add a member, or rotate their token. |
| `intagent login --url <server>` | Save your token for a server, mode 0600, in your user config directory (`~/.config/intagent/` on Linux, `~/Library/Application Support/intagent/` on macOS). `--share-prompts off` keeps your prompts off the board. |
| `intagent init` | Enrol a repository and wire up its agents. |
| `intagent doctor` | Check every link between this worktree, its agents and the server. |
| `intagent board [--watch 5s] [--json]` | Who is working on what in this repository. |
| `intagent check <path>...` | Who else is working on these paths. |
| `intagent declare [-x] -m <why> <glob>...` | Declare intent for this worktree (`-x`: exclusive). |
| `intagent release [<glob>...]` | Release intents. |
| `intagent note <member\|claim\|path> <text>` | Leave a note for another member's agents. |
| `intagent watch` | Keep a worktree on the board for agents without hooks. |
| `intagent guard` | Pre-commit check against teammates' exclusive intents. |
| `intagent hook [<agent>]` / `intagent mcp` | Called by agents; the hook recognises the agent from what it sends. |

Environment: `INTAGENT_URL` and `INTAGENT_TOKEN` override the configuration, `INTAGENT_DISABLE=1` turns intagent off,
`INTAGENT_FAIL=closed` refuses edits in enrolled repositories while intagent cannot check them (the server is
unreachable or too busy to check them, or the setup cannot be read; the default is to fail open), `INTAGENT_TIMEOUT`
bounds each request (default `2s`). A whole hook run, git included, takes at most 8 seconds, and at a session's end 4
for Claude Code, Gemini CLI and Cursor and 2 for other agents (less if `CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS` asks),
under the timeouts `intagent init` gives the agents, so a longer `INTAGENT_TIMEOUT` is cut short there.
`intagent check`, the MCP `check_paths` tool and `intagent guard` send more than 200 paths in several checks. Under
`INTAGENT_FAIL=closed`, a hook older than this release lets an edit the server is too busy to check go ahead, with a
note to its agent.

## Security model

- What teammates see: your agents' sessions, the files your worktree changed against the default branch, the intents
  your agents declare, and, unless you turn it off with `intagent login --share-prompts off`, the first line of each
  session's first prompt as its task. Only repositories whose team committed `.intagent.json` are reported.
- Tokens are stored on the server only as SHA-256 hashes and compared in constant time. The member is always taken
  from the token, never from the request.
- Writes accept only bearer tokens. Signing in to the dashboard exchanges the token for a session cookie (HttpOnly,
  `SameSite=Strict`, `Secure` over HTTPS) that can only read, so a page in another tab cannot act on your behalf.
  The cookie is a MAC of the token's hash under a key kept in the data directory: it is not the token, cannot be
  forged from the team file, survives a restart and ends when the token is rotated.
- Text from one member's agent reaches another's only quoted, stripped of control and invisible formatting
  characters, collapsed to one line, capped in length, and introduced as information rather than instructions.
  Paths and patterns containing control, formatting or line-separator characters are rejected; branch, agent and
  tool names are reduced to a single token; webhook messages escape Slack markup. Notes are rate-limited.
- The hook fails open with a short timeout: intagent never stops an agent because intagent is down, unless a team
  asks for `INTAGENT_FAIL=closed`.

## What intagent deliberately does not do

It does not merge, rebase or resolve conflicts (a merge queue does that), decide whether work is correct (tests and
CI do that), or run and schedule agents (people and orchestrators do that). It makes agents and their people aware of
each other, at the moment it matters.

## Development

```sh
make check         # golangci-lint and go test -race
make e2e           # two real Claude Code agents against a local server (needs an authenticated claude CLI)
make e2e-codex     # the real Codex CLI against a scripted model: offline, free (needs npm and jq)
make e2e-copilot   # the real GitHub Copilot CLI against a scripted model: offline, free (needs npm)
make e2e-gemini    # the real Gemini CLI against a scripted model: offline, free (needs npm)
make dist VERSION=v0.1.0    # release archives for Linux, macOS and Windows, with SHA256SUMS
make image VERSION=v0.1.0   # the server's container image
```

`scripts/loadgen` drives a running server the way a team does, and reports how it held up: agents in their own
worktrees sending every kind of hook, dashboards reloading the board as the page does and holding their streams, and
for each endpoint the latencies, the errors and the edits a hook would have let through unchecked, with the server's
memory and CPU. It is not part of `make check`. Its defaults are the scale intagent is meant for: 200 members with
five agents each, 30 repositories, 100 dashboards and 300 streams.

```sh
go build -o /tmp/lg/intagent ./cmd/intagent
go run ./scripts/loadgen -setup -intagent /tmp/lg/intagent -team /tmp/lg/team.json -tokens /tmp/lg/tokens.json
/tmp/lg/intagent serve --config /tmp/lg/team.json --data /tmp/lg/data --addr 127.0.0.1:7400 & echo $! >/tmp/lg/pid
go run ./scripts/loadgen -tokens /tmp/lg/tokens.json -pidfile /tmp/lg/pid -rate 200 -duration 60s
kill "$(cat /tmp/lg/pid)"
```

`go run ./scripts/loadgen -h` lists the rest: the shape of the team and its footprints, the rate, a warm-up, and
flags that model older clients and dashboards.

Pushing a tag `vX.Y.Z` runs the release workflow: it tests, publishes a GitHub release with those archives, and
pushes the server image to `ghcr.io/<owner>/intagent` for amd64 and arm64.

The design is in [docs/design.md](docs/design.md).
