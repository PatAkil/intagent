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
5. **Fail open.** If the server is down or slow, the hook allows the edit and says nothing. intagent must never be the
   reason an agent cannot work.
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

The server's sweeper emits `session.stalled` and `session.gone` events once per transition, so the dashboard and the
owner's next session see them.

## Awareness and conflicts

When an agent is about to write a file, the hook asks the server to check the path against every other claim in the
same repo. Matches are ranked:

| Severity | Condition | Default action |
|---|---|---|
| `block` | An **active** claim holds an **exclusive** intent covering the path. | `deny`: the edit is refused with a reason naming the owner, their summary and how to coordinate. |
| `overlap` | Another claim has changed the path, or holds a shared intent covering it, or a dormant claim holds an exclusive one. Also another live session in the same claim. | `bump`: the first attempt is refused with the explanation; a retry of the same path is allowed. The agent cannot miss it, and is never stuck. |
| `nearby` | Another claim has changed or declared something in the same area. | `warn`: context is added once per area per session. |

Actions are configurable per severity on the server: `deny`, `ask` (the person decides), `bump`, `warn`, `off`.

Awareness is symmetric. When an agent touches a file another claim has also touched, the other claim gets an inbox
item, delivered at its agent's next hook. Inbox items (notes and alerts) are delivered once per session through
`additionalContext` on `SessionStart`, `UserPromptSubmit` and `PostToolUse`.

Every acknowledgement is remembered per session, so an agent hears about a given overlap once, not on every edit.

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

At session start, after shell commands (at most every 15 seconds), at `Stop` and at `SessionEnd` the hook computes the
worktree's real footprint:

- the default branch from `refs/remotes/origin/HEAD` (falling back to `origin/main`, `origin/master`, `main`, `master`);
- `git merge-base HEAD <default>`;
- `git diff --name-only <merge-base>` (committed and uncommitted changes);
- `git ls-files --others --exclude-standard` (new files).

The server replaces the claim's git-derived footprint with this list. Hook-recorded touches newer than the
reconciliation stay. Once a branch is merged and the worktree is clean, the footprint is empty and the claim releases
itself.

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
internal/server        HTTP API, auth, SSE hub, sweeper, snapshot persistence, webhooks
internal/client        HTTP client and configuration (env, user config, repo config)
internal/gitx          repo identity, worktree root, branch, footprint, areas
internal/hook          adapters (Claude Code, Copilot CLI, Codex, Cursor, Gemini CLI): vendor hook JSON in,
                       normalised event out, vendor response back; detected from the payload
internal/mcp           a minimal stdio MCP server and intagent's tools
internal/web           the dashboard, embedded
```

The server keeps the board in memory behind one mutex. Each mutation marks the board dirty; a writer goroutine saves
an atomic snapshot (write, fsync, rename) at most once a second and on shutdown. Team scale is dozens of members and
a few hundred sessions, which this handles with room to spare.

Reads of a repository's board are shared, because every open dashboard reloads it a moment after each of its events.
A request joins the build of the board that has not read the board yet, so no answer is older than its request (a
read after a write sees the write), and each build is encoded and compressed once for all who joined it. Builds of
one repository start at least 250 ms apart, or twice as long as the last one took, and one build runs at a time
across the server, so a hook waits behind at most one.

## API

All endpoints take and return JSON and require `Authorization: Bearer <token>`, except `/healthz`.

| Method and path | Used by | Purpose |
|---|---|---|
| `POST /v1/hook` | hooks | One normalised lifecycle event in, a decision and context out. |
| `POST /v1/intents` | MCP, CLI | Declare intents for a claim. Returns overlaps. |
| `POST /v1/intents/release` | MCP, CLI | Release some or all intents. |
| `POST /v1/check` | MCP, CLI, guard | Who else claims or touched these paths. Read-only. |
| `POST /v1/notes` | MCP, CLI | Send a note to a claim or a member. |
| `GET /v1/board` | CLI, dashboard | Every claim and session in a repo, with derived states. Gzip when the client takes it, and a weak `ETag` for `If-None-Match`. Its `epoch` changes when the server restarts. |
| `GET /v1/repos` | dashboard | The repositories with claims, each with the server's `epoch`. |
| `GET /v1/stream` | dashboard | Server-sent events. |
| `GET /v1/whoami` | CLI | The member a token belongs to. |

## Security model

- Tokens are stored on the server as SHA-256 hashes and compared in constant time.
- The dashboard exchanges a member's token for a read-only session cookie: an HMAC of the token's hash under a key
  kept in the data directory (`ui.key`, mode 0600), never the token itself.
- `intagent serve --tls-cert --tls-key` serves HTTPS directly; behind a proxy, `X-Forwarded-Proto: https` marks the
  cookie `Secure`.
- All member-supplied text is stripped of control and formatting characters (bidi overrides, zero-width), collapsed
  to one line and length-capped on the server, and quoted and framed as teammate data when rendered into an agent's
  context. Paths and patterns, which are shown unquoted, are rejected if they hold control, formatting or line
  separator characters or exceed 1024 bytes; branch, agent, tool and repository names are reduced to one token with
  no spaces, quotes or angle brackets.
- Webhook messages escape `&`, `<` and `>`, which Slack reads as mentions and links.
- Notes are rate-limited per member.
- Request bodies are capped. Paths are validated.
- An answer that makes no progress for 15 seconds is cut off, so a client that stops reading mid-answer (a laptop
  put to sleep) does not hold the server's memory; a slow client that keeps reading gets all of it.
- The hook fails open with a short timeout. `INTAGENT_FAIL=closed` turns an unreachable server, or a setup intagent
  cannot read, into a refusal of edits in enrolled repositories, for teams that want it. A whole hook run, git
  included, has 8 seconds (2 at a session's end), under the timeouts `init` gives the agents, which would otherwise
  kill it and let the edit through; git gets at most half of what is left, so the event still reaches the server.

## What it deliberately does not do

- It does not merge, rebase or resolve conflicts. A merge queue does that.
- It does not decide whether work is correct. Completion gates (tests in a `Stop` hook, CI) do that.
- It does not run or schedule agents. People and orchestrators do that; intagent makes them aware of each other.
