# intagent design

intagent ("intent for agents") gives every coding agent on a team a live view of what every other agent on the
team is changing, and gives the team a view of every agent. It is one Go binary with no third-party dependencies:
a small team server, a hook handler that agents call on every lifecycle event, and a stdio MCP server that lets an
agent declare what it is about to do.

## The problem it solves

Several people each run several agents (Claude Code, Codex, Cursor, Copilot CLI, Gemini CLI) in one monorepo.
Each vendor's multi-agent features stop at one person's account, so agents belonging to different people only meet
at the pull request.
By then two agents may have rewritten the same file, renamed the same API in different ways, or done the same task
twice. intagent moves that meeting point to the moment an agent is about to edit a file.

## Principles

1. **Automatic, not optional.** Agents connect through hooks checked into the repo. An agent that has to remember to
   report will forget; a hook runs every time.
2. **Awareness at the moment of the edit.** Context injected at session start is forgotten. The warning that matters
   arrives in the `PreToolUse` hook for the exact file being written.
3. **Facts over claims.** What an agent says it will change (intent) is useful early but unreliable. What it has
   changed (footprint) comes from the hooks and from git, and is reconciled against `git diff` so reverted or merged
   work disappears by itself.
4. **Leases, not locks.** Nothing an agent holds outlives the agent for long. A crashed or hung agent stops blocking
   anyone after the stall timeout; its unmerged work stays visible as a warning.
5. **Fail open.** If the server is down or slow, the hook allows the edit; the agent hears afterwards which of its
   edits went ahead unchecked. intagent must never be the reason an agent cannot work.
6. **Untrusted text is data.** Everything one agent reports reaches other agents' context. It is sanitised, capped and
   framed as data from teammates, never as instructions.

## Vocabulary

| Term | Meaning |
|---|---|
| **Team server** | `intagent serve`. One per team. Holds the board in memory, persists it to a snapshot file. |
| **Member** | A person. Identified by their bearer token. |
| **Session** | One agent run: a Claude Code session, a Codex session, a Cursor conversation, a Copilot CLI or Gemini CLI session, or an `intagent watch` loop. Has a liveness state. |
| **Claim** | The unit of ownership: one per member, host and worktree. Holds intents, footprint, a task summary and an inbox. Sessions in the same worktree share a claim, because they write the same files. |
| **Intent** | A declared path or glob with a mode (`shared` or `exclusive`) and a summary. "I am about to change `services/payments/**`." |
| **Footprint** | The files a claim has actually changed relative to the default branch: added by `PostToolUse`, reconciled against git at session start, stop and end. |
| **Area** | The package a file belongs to (nearest `go.mod`, `package.json`, `Cargo.toml`, `AGENTS.md` and similar, or configured globs). Used for "nearby" awareness. |
| **Note** | A short message from one claim to another, delivered into the target agent's context at its next hook. |
| **Event** | An append-only record of what happened, streamed to the dashboard. |

## Liveness

A session's state is derived from its last event and the clock:

| State | Meaning | How it is reached |
|---|---|---|
| `working` | A turn is in progress. | `UserPromptSubmit`, `PreToolUse`, `PostToolUse`. |
| `waiting` | The turn ended; the agent waits for its person. | `Stop`. |
| `stalled` | Working, but silent for longer than `stall_after` (10 min), or inside one tool call for longer than `tool_stall_after` (45 min). | Derived. |
| `gone` | Silent for longer than `idle_after` (2 h) without a clean end: crashed, killed, or abandoned. | Derived. |
| `ended` | The session ended cleanly. | `SessionEnd`. |

A claim is **active** when at least one of its sessions is `working` or `waiting`. Otherwise it is **dormant**: its
footprint still produces warnings, but its exclusive intents stop blocking. A claim with no live session, no intents
and an empty footprint is released. Claims with no activity for `forget_after` (7 days) are removed.

What the board keeps is bounded whatever clients send, and nothing past a bound refuses a hook. A claim keeps the 20
sessions that ended or went gone last. The board keeps at most `max_sessions` (20,000) sessions: an event from a new
session it has no room for is answered without storing anything, its edits judged as a check judges them (a
reservation still refuses; what would refuse once, a bump or a question to an agent that cannot ask, warns instead,
since nothing remembers it was said), and its session start says its teammates will not hear of its work; the
repository's stats count such hooks (`unstored`). Each sweep makes room by dropping the sessions not live that went
silent longest, down to nine tenths of the bound. Past `max_dormant_claims` (20,000) claims with no live session, each
sweep forgets up to 200 of those quiet longest that hold no intent, announced in one `claim.forgotten` per repository
that names no claim. A footprint counts each file's path and area, and 96 bytes more, against two budgets: 512 KB for
one claim (2000 files of a large monorepo's paths take about 350 KB) and 32 MB for all of one member's claims, a fleet's
included. A footprint keeps the files its budgets take, the first in the order the client sent them, which puts first
the files it would least want left out, and is marked truncated.

The server's sweeper emits `session.stalled` and `session.gone` events once per transition, so the dashboard and the
owner's next session see them. A webhook hears of them at most once a second, everything waiting in one message, so a
network blip that stalls hundreds of agents at once reads as one summary rather than hundreds of alarms. A session that
was waiting for its person when it turned gone is marked `idle`: it is not stuck, and the webhook leaves it out unless
the team asks for it (`"webhook": {"idle": true}`). A session is announced `session.recovered` only when it reports
again after a stall or gone was announced: one back before the sweeper noticed has recovered from nothing.

The time the server was down does not count as its agents' silence: a restored board moves each session's last report,
and the start of the tool it is in, on by the time the server was down, never past the restart. A board that does not
change is not saved again, so the server also notes in `board.json.alive`, every 30 seconds and when it stops, that it
runs: the time it ran on after its last save, hearing nothing from an agent, still counts. Agents still at work are not
announced stalled by the first sweep, and their exclusive intents keep refusing. The cost is that an agent that died
just before the outage holds its reservation for up to `stall_after` after the restart, and up to 30 seconds more after
a crash.

## Awareness and conflicts

When an agent is about to write a file, the hook asks the server to check the path against every other claim in the
same repo. Matches are ranked:

| Severity | Condition | Default action |
|---|---|---|
| `block` | An **active** claim holds an **exclusive** intent covering the path. | `deny`: the edit is refused with a reason naming the owner, their summary and how to coordinate. |
| `overlap` | Another claim has changed the path, or holds a shared intent covering it, or a dormant claim holds an exclusive one. Also another live session in the same claim. | `bump`: the first attempt is refused with the explanation; a retry of the same path is allowed. The agent cannot miss it, and is never stuck. |
| `nearby` | Another claim has changed or declared something in the same area. | `warn`: context is added once per area per session. |

Actions are configurable per severity on the server: `deny`, `ask` (the person decides), `bump`, `warn`, `off`.

Awareness is symmetric. When an agent touches a file another claim has also touched, the other claim gets an inbox item,
delivered at its agent's next hook. Inbox items (notes and alerts) are delivered once per session through
`additionalContext` on `SessionStart`, `UserPromptSubmit` and `PostToolUse`. Only claims still listening are queued
anything: those with a live session, or active within `dormant_for` (a day). A claim quiet for longer counts only as
nearby work, so nothing is queued for it, nor remembered of what it would have been told; an agent that comes back to it
hears of teammates' work from its greeting and its checks, and may hear again of a change it heard of before it left. An
alert names at most five files and counts the rest, and a claim remembers being told of at most five files of each
teammate's claim: past those, a file that comes back into the teammate's changes is told again, so what claims sharing a
large footprint remember grows with the pairs of them, not with the files. A note to a member goes to their claims still
listening, and to one none of whose claims listens, to the claim they were last active in, rather than to every worktree
they left. An inbox holds 50 items; a full one lets go first of an item a session was already shown, then of the oldest
item of the sender holding the most, an alert before a note when senders tie, so a flood of one teammate's alerts or
notes pushes out their own items rather than another's note the agent has not heard yet. The sweeper drops inbox items a
day old, which no session is shown any more, and the alerts a claim that no longer listens remembers having heard; and
once a claim is removed, the alerts other claims remember of it, which nothing can match again since a claim's ID is
never reused.

Every acknowledgement is remembered per session, so an agent hears about a given overlap once, not on every edit. A
collision is announced (a `conflict` activity, for the dashboard and webhooks) under the teammate whose work decided the
answer, and names at most four others it newly ran into, a member's many worktrees with the same news in one line,
then counts the rest.

## Intents

An agent declares intent through the MCP tool `declare_intent` before a multi-file change:

```json
{"summary": "Move retry policy into its own package", "paths": ["services/payments/retry/**", "services/payments/client.go"], "mode": "exclusive"}
```

The server answers with the overlaps it already knows about, so the agent can change plan before it writes anything.
An exclusive intent that overlaps another active exclusive intent is refused; the agent can declare it shared
instead. Intents end when released, when the claim is released, or (for enforcement) when the claim goes dormant.

The first line of a session's first prompt becomes the claim's task summary unless the member turns prompt sharing
off (`intagent login --share-prompts off`). An explicit `declare_intent` summary replaces it.

## Footprint from git

At session start, after shell commands (at most once in each 15-second window per worktree, whichever of its sessions
ran them), at `Stop` and at `SessionEnd` (unless a footprint of the worktree reached the server whose scan began in the
last 15 seconds, after every shell command that did not scan, as the `Stop` just before sends one) the hook computes
the worktree's real footprint:

- the default branch from `refs/remotes/origin/HEAD` (falling back to `origin/main`, `origin/master`, `main`, `master`);
- `git merge-base HEAD <default>`;
- `git diff --name-only <merge-base>` (committed and uncommitted changes);
- `git ls-files --others --exclude-standard` (new files).

The diff runs on a private copy of the index (`GIT_INDEX_FILE`): git diff writes back the index it refreshed, under
`.git/index.lock`, even with `GIT_OPTIONAL_LOCKS=0`, and that lock refuses the person's and other agents' `git add` while
it is held. The refreshed copy is kept in intagent's cache directory, named by the real index it began as, and the next
scan starts from it while the real index is unchanged, so after a formatter only the first scan checks the content of
the files it touched. Git out of time is stopped with SIGTERM, so it removes its lock files (Windows has no such signal:
there it is killed, and the private index is what keeps the real one unlocked).

The files the repository's `ignore` patterns match are left out first. A footprint keeps at most 2000 files; past
that, the client keeps one file of each changed area, then the files the branch has not committed, a new package
included, then the branch's commits (`git diff --name-only <merge-base> HEAD`), then the files of each directory the
worktree added whole with more than 500 files (a `.venv`, build output nobody ignored), the smallest first. Each group
goes in path order, and the client sends them in that order, since the server keeps the first files it can. A
directory added whole whose files do not all fit goes in `dirs`, one entry for the files it leaves out. The server
keeps the valid, distinct ones among the first 200 and does not judge them yet, so teammates are not told about the
files they stand for.

The server replaces the claim's git-derived footprint with this list. Hook-recorded touches newer than the
reconciliation stay. A new file in the list that falls inside a teammate's exclusive intent is reported to every live
session of the claim, not only the one whose hook sent the scan, since that scan may follow another session's command.
Once a branch is merged and the worktree is clean, the footprint is empty and the claim releases itself.

## Identity

- **Repo**: the normalised `origin` URL (`git@github.com:Acme/Mono.git` → `github.com/acme/mono`), or `local/<dir>`
  without a remote, or the `repo` field of `.intagent.json`.
- **Paths**: relative to the worktree root, forward slashes, cleaned. Absolute paths and `..` are rejected.
- **Member**: from the bearer token. The server never trusts a member name sent by a client.
- **Session**: the agent's own session id, namespaced by member and agent kind.
- **Claim**: member + host + worktree path, with a short random id.

## Components

```
cmd/intagent           main: wires os.Args, stdio and signals into internal/cli
internal/cli           the commands: serve, token, login, init, doctor, board, check, declare, release, note,
                       hook, mcp, watch, guard, demo, version
internal/board         the domain: sessions, claims, intents, footprint, policy, liveness, inbox, stats.
                       Pure; time is passed in
internal/glob          ** glob matching for intents and areas
internal/server        HTTP API, auth, admission of large requests, shared board reads, SSE hub, sweeper,
                       snapshot persistence, load status, webhooks
internal/client        HTTP client and configuration (env, user config, repo config)
internal/gitx          repo identity, worktree root, branch, footprint, areas
internal/hook          adapters (Claude Code, Copilot CLI, Codex, Cursor, Gemini CLI): vendor hook JSON in,
                       normalised event out, vendor response back; detected from the payload
internal/mcp           a minimal stdio MCP server and intagent's tools
internal/web           the dashboard, embedded
```

The server keeps the board in memory behind one mutex. Each mutation marks the board dirty, and a goroutine of its own
saves an atomic snapshot (write, fsync, rename) of a changed board, spaced by what the last save cost: nine times its
duration after it started, between 1 and 30 seconds. An unclean stop loses what changed since the last save, which on a
large board is a few seconds' work. A snapshot shares the claims' footprints and alerts with the board, which copies one
before it next changes it, so the lock is held only to copy the claims and sessions, and it is written to disk a claim
at a time. Saves that fail are tried again after 1, 2, 4, 8 and 16 seconds, then every 30; the server logs the first
failure, then one a minute, then the recovery, and `/healthz` says so. Stalled sessions are looked for on a goroutine of
their own, so a slow or stuck disk does not delay the news. A clean stop abandons a save in flight, waits for the
requests in flight, and saves once more; `serve` exits non-zero if that save fails. Each save keeps the snapshot it
replaces as `board.json.prev`, a hard link made before the new one is renamed in. At start, the temporary files of saves
that were killed halfway are removed, and a snapshot that is cut short or damaged is set aside as
`board.json.corrupt-<unix time>` and `board.json.prev` put in its place and restored, or nothing: a server that will not
start leaves every agent unchecked, and one stopped before its next save restores the same again. A snapshot of a newer
format, or one that cannot be read, still stops it. Beside its maps the board keeps indexes, rebuilt from the snapshot
on a restart: each repository's claims, each claim's sessions, and for each claim the latest change in each area it
changed files in and its changed paths in order. A check of a path then looks at
each claim of its repository once, asks the sessions of only the claims that matter to it whether they are live, and
finds nearby work by a lookup rather than a walk of every file; a declared intent is matched against the files under
the directory it is rooted in. What a check costs grows with the claims of its repository, not with every session and
file on the server. The scale it is meant for is about
200 members running a thousand agent sessions in 30 repositories, with 100 dashboards open; `scripts/loadgen` drives a
running server that way, with clients and dashboards that behave as intagent's do, and reports how it held up.

Reads of a repository's board are shared, because every open dashboard reloads it a moment after each of its events.
A request joins the build of the board that has not read the board yet, so no answer is older than its request (a
read after a write sees the write), and each build is encoded and compressed once for all who joined it. Builds of
one repository start at least 250 ms apart, or twice as long as the last one took to read the board, and one build
reads at a time across the server, so a hook waits behind at most one read. A read holds the board's lock only while
it copies the claims, files and sessions it shows; it orders the files and caps them once it has let go (on a
repository of 300 claims at 2000 files each, 13 ms of a 310 ms read). Encoding takes no lock and runs outside that
limit.

The dashboard follows a repository through a server-sent event stream. Each activity is encoded once, and the server
keeps the last 1024 publishes, up to 16 MB of them, in one ring that every stream reads at its own pace, so publishing
costs the same however many dashboards are open. A stream writes what is new in one write, then waits 100 ms before it
takes more, so a burst wakes each stream ten times a second rather than once per activity. A stream that falls further
behind than the ring keeps is ended and catches up with `Last-Event-ID`; when the last stream closes, the ring lets go
of what it held. The board keeps the team's last 300 activities; when a reconnecting stream missed one it no longer
keeps, the stream starts with `event: gap` and `data: {"after":N}`, and the dashboard marks the hole in its feed and
reloads the board. The snapshot keeps, by repository, the newest activity the board let go of, so after a restart a
dashboard is told of a gap only when it has one. A stream whose client takes no part of a write for 15 seconds is ended,
and on Linux the kernel gives up on a connection whose peer acknowledges nothing for a minute, so a dashboard that went
away does not hold the server's memory. Streams are capped at 20 per member, 100 without a token on a board anyone may
read, and 500 in all (`--max-member-streams`, `--max-public-streams`, `--max-streams`, where 0 allows none); a stream
counts until its handler returns, and one over a cap is answered 429 with `Retry-After`, which the dashboard follows
with its own backoff. A change to the team file ends only the streams opened with a token it removed or rotated, so
adding a member disconnects nobody. When the server ends streams together, at shutdown say, each first tells its
dashboard to reconnect after its own delay of 3 to 8 seconds, so they do not all come back and reload the board at once.

## API

All endpoints take and return JSON and require `Authorization: Bearer <token>`, except `/healthz`.

| Method and path | Used by | Purpose |
|---|---|---|
| `POST /v1/hook` | hooks | One normalised lifecycle event in, a decision and context out. The conflicts an edit's answer lists are those that block or overlap, up to 200, and the 20 newest nearby; `more_conflicts` counts the rest. |
| `POST /v1/intents` | MCP, CLI | Declare intents for a claim. Returns overlaps. |
| `POST /v1/intents/release` | MCP, CLI | Release some or all intents. |
| `POST /v1/check` | MCP, CLI, guard | Who else claims or touched these paths, the first 200 of them (`unchecked` counts the rest, and `text` says so; intagent's CLI, MCP tool and guard send more in several checks). Read-only. |
| `POST /v1/notes` | MCP, CLI | Send a note to a claim or a member. |
| `GET /v1/board` | CLI, dashboard | Every claim and session in a repo, with derived states. Gzip when the client takes it, and a weak `ETag` for `If-None-Match`. Its `epoch` changes when the server restarts, and `server` is there while agents' edits go ahead unchecked (below). With `format=text`, the board as text; adding `limit`, `host` and `worktree` gives an agent at most `limit` claims (16 KB), those sharing files or areas with its own first, as the MCP `team_board` tool shows them. |
| `GET /v1/repos` | dashboard | The repositories with claims, each with the server's `epoch`. |
| `GET /v1/stream` | dashboard | Server-sent events. |
| `GET /v1/whoami` | CLI | The member a token belongs to. |
| `GET /healthz` | monitors | Up, and whether agents' edits go ahead unchecked: `ok`, `version`, `degraded`, `since`, `pre_edits_60s` and `unchecked_60s`; with a data directory, `snapshot`: `ok`, `saved_at`, and while saves fail `failing_since`, `attempts` and `error` (the operation and the system's error, no paths). Always 200, except with `?strict=1`: 503 while degraded or while saves fail. |

## When the server falls behind

A hook client waits a short time and then lets the agent go ahead, so under load the server can get to an event
after its agent has stopped waiting. Every request carries `X-Intagent-Timeout`, how long the client waits in
milliseconds. The server notes when a hook arrived, before reading its body (a large one may wait for a slot, below),
and, once it holds the board's lock, asks whether the request has waited longer than that, less min(300 ms, a
quarter), or whether its connection has closed after at least a second (a proxy that half-closes a connection closes
the request's context while it still waits). Then:

- a late `pre_edit`, which only asks whether an edit may go ahead, changes nothing: nothing is acknowledged,
  counted, recorded or announced, and the answer is an allow marked `unchecked`, which a client still waiting treats
  as no answer;
- any other late event reports a fact (a tool started, a session alive, an edit made) and is recorded, but nothing is
  delivered: notes, alerts and held-back context wait for the session's next answer.

An answer can also miss its agent for reasons the server cannot see. A `post_edit` whose `tool_use_id` the board did
not see start, or saw refused, is judged when it arrives, without acknowledging anything, and its answer is marked
`checked_after`: what the agent would have been told reaches it as information, what a refusal it never heard had
spent is given back so that its next attempt is refused again, and a change inside an active reservation is recorded
as a breach. The hook keeps its own ledger of the edits that went ahead without an answer, per worktree and session,
and tells them at the session's next answer the agent reads, less the edit a `checked_after` answer reports.

The server counts, per second over the last minute, the `pre_edit`s that arrive and those whose agent went ahead
unchecked, including those it let through unchecked when it was too busy with large requests to check them. It is
degraded once more than 5% of the last 10 seconds' (and at least 3) went unchecked, and recovers once it has been
degraded for 30 seconds and fewer than 1% of the last 30 seconds', and of the last 10 seconds', did: traffic falls
in a stall, as agents wait out their timeouts, and a share over a longer window alone would let the state flip every
second. It says so in `/healthz`, on the dashboard (a `status` event on the stream, and `server` in `/v1/board`
while degraded), in its log and, if asked, by webhook.

## Security model

- Tokens are stored on the server as SHA-256 hashes and compared in constant time.
- The dashboard exchanges a member's token for a read-only session cookie: an HMAC of the token's hash under a key
  kept in the data directory (`ui.key`, mode 0600), never the token itself.
- `intagent serve --tls-cert --tls-key` serves HTTPS directly; behind a proxy, `X-Forwarded-Proto: https` marks the
  cookie `Secure`. Each hook is a new process and a new connection, so with TLS each pays for a full handshake: ECDSA
  certificates go first, a server with only an RSA one says at startup what that costs, and a handshake not finished
  2 seconds after its connection was accepted is abandoned, since its hook has already failed open.
- All member-supplied text is stripped of control and formatting characters (bidi overrides, zero-width), collapsed
  to one line and length-capped on the server, and quoted and framed as teammate data when rendered into an agent's
  context. Paths and patterns, which are shown unquoted, are rejected if they hold control, formatting or line
  separator characters or exceed 1024 bytes; branch, agent, tool and repository names are reduced to one token with
  no spaces, quotes or angle brackets.
- Matching a teammate's pattern against a path is bounded in two ways, since one member's patterns are matched under
  the board's lock against every path every agent edits. Paths have at most 64 segments of at most 255 bytes, and
  patterns at most 32 segments, 4 of them `**`, with wildcard segments of at most 64 bytes; a snapshot's intents
  outside these bounds are dropped when it is restored, with a log line. And each call that holds the lock may match
  only so much, counted the same way on every run (a unit for each comparison of two segments, and for one with
  wildcards `len(p)·len(s)/32` more, 2 million in all). On the target board an edit of 200 files uses about a
  seventh of the bound, and a worktree arriving with 2000 changed files or a declaration of 50 patterns about a
  fortieth: the files a worktree arrives with are matched against a teammate's intent only where they fall under the
  directory it is rooted in. Patterns rooted nowhere, such as `**/*_mock.go`, are matched against every file, and
  about a hundred of them in one repository use the bound up on such an arrival. Past it, the call compares no more
  claims and lets through what it did not compare. A worktree's changes are compared with teammates' reservations
  before teammates are alerted of them, so a change inside a reservation is reported however much the alerts would
  cost. The edit's agent, or the session whose worktree's changes were being compared, is told intagent stopped
  before it had compared everything; a check or a declaration says so in its answer (`partial`); a declaration
  refuses an exclusive intent it could not compare with every reservation, which two claims could otherwise hold at
  once; and the repository's stats count each such call (`partial`). The costliest patterns at the bounds then hold the
  lock for about a tenth of a second per call, however many worktrees declare them: 50 to 85 ms for a check of 200
  paths, which without the bound took 0.4 to 1.5 s (and, before the bounds on shape, about a minute), and about 85 ms
  for a worktree arriving with 2000 changed files.
- Webhook messages escape `&`, `<` and `>`, which Slack reads as mentions and links.
- Notes are rate-limited per member.
- Request bodies are capped at 1 MB. Paths are validated. A body over 16 KB (a footprint, in practice) is charged to
  its member's budget of 4 MB a second, with a burst of 32 MB for a fleet's session starts, from its `Content-Length`
  before it is read (429 past it), and only as many are decoded and handled at once as the server has CPUs (503 after
  a second's wait). The hook then sends its session start, stop or session end again at once without the footprint:
  the event is small and never metered, and lost, a stop would leave its session working, to be announced as stalled,
  and a session's end would leave it live, its reservations refusing teammates' edits, for hours. The footprint is
  not recorded as sent, so the next scan sends it. A pre_edit is never answered 429 or 503: its agent should hear
  that its edit was not checked. Most are a few hundred bytes and not metered. One over 16 KB (about 200 paths: a
  codemod's patch) draws on a budget of its own, which footprints do not spend, and when that budget or the wait for
  a slot runs out the server answers it as a late one: an allow marked `unchecked`, counted towards being degraded.
  Under `INTAGENT_FAIL=closed` the hook refuses it, as any edit the server did not check; otherwise the edit goes
  ahead and its agent is told, from the ledger or by its `post_edit`'s check. The answer's context tells it at once
  to the agent of a hook older than the mark, which lets the edit go ahead even under `INTAGENT_FAIL=closed`. The
  server knows a pre_edit by how its body starts, `{"kind":"pre_edit"`, as intagent's clients write it, and refuses a
  body that starts so and turns out to be another kind. Clients send only a prompt's first line, which is all the
  board reads. Footprints and prompts are cut to size before the board's lock is taken: a footprint to its first 2000
  entries, duplicates and invalid paths included.
- An edit is checked on the first 200 paths it names, and past that its agent is told how many were not checked. A
  check is too: its answer counts the paths past the first 200 in `unchecked`, and its text says so.
  `intagent check`, the MCP `check_paths` tool and `intagent guard` send more in checks of 200. Older clients send a
  whole commit in one check and read only its conflicts, so they get what older servers gave them: the first 200
  paths checked and the rest let through unsaid (a refusal would let the whole commit through, or under
  `INTAGENT_FAIL=closed` refuse every large one). Each path costs a look at each of the repository's claims under
  the board's lock: 200 paths on a repository of 300 claims of 50 files, every twentieth at 2000, hold it for about
  25 ms. A post_edit's paths, which only join the claim's files, are kept up to 2000, and an activity lists 200 of them
  and counts the rest. Checks and declarations are paced per member and worktree, one at a time and five a second with
  a burst of 20 (429 past that), since each holds the board's lock for as long as its paths and patterns take; an
  orchestrator's agents, each in its own worktree, are paced apart. Hooks are never paced, since a fleet's agents
  share one token.
- A client has 15 seconds to send a whole request and 16 KB for its headers, and past 4096 open connections
  (`serve --max-connections`) the server closes new ones as soon as it accepts them, so a client that sends slowly, or
  opens many connections, with a token or without, cannot use up the server's file descriptors and memory. A hook
  turned away fails open at once instead of waiting out its timeout.
- An answer that makes no progress for 15 seconds is cut off, so a client that stops reading mid-answer (a laptop
  put to sleep) does not hold the server's memory; a slow client that keeps reading gets all of it.
- The hook fails open with a short timeout. Only a refused connection is tried again, after 100, 200 and then every
  400 ms while more than half a second of the request's time is left: a server restarting closes its port for a
  moment, and nothing of the request reached it, while a request reset or timed out may have been decided, and an
  answer it spent must not be asked for twice. A name that does not resolve, or a network out of reach, is not tried
  again: a restart does not cause them. A stamp in the user's cache directory notes when refusals began; after 10
  seconds of them, hooks on that machine try once, until the server answers one, so a server that is down costs only
  the first few edits their time. `INTAGENT_FAIL=closed` turns an unreachable server, an answer the server
  could not check in time or was too busy to check, or a setup intagent cannot read, into a refusal of edits in
  enrolled repositories, for teams that want it. A whole hook run, git included, has 8 seconds, and at a session's
  end 4 for Claude Code, Gemini CLI and Cursor and 2 for other agents (less if
  `CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS` asks), under the timeouts `init` gives the agents, which would otherwise
  kill it and let the edit through; git gets what the server request leaves of that, and at least half, so a slow git
  cannot keep the event from the server.

## What it deliberately does not do

- It does not merge, rebase or resolve conflicts. A merge queue does that.
- It does not decide whether work is correct. Completion gates (tests in a `Stop` hook, CI) do that.
- It does not run or schedule agents. People and orchestrators do that; intagent makes them aware of each other.
