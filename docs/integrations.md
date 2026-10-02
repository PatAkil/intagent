# Agent integrations

Every agent silently ignores hook output it does not understand, so intagent's adapters were written against
payloads and behaviour observed in live runs, not against documentation alone. This page records what each agent
does and what `intagent init` installs for it.

## Claude Code

Verified against Claude Code 2.1.288, headless (`claude -p`) and with project settings.

**Installed by `intagent init`:** hooks in `.claude/settings.json`, the MCP server in `.mcp.json`, and the
permission `mcp__intagent__*`.

| Event | Matcher | intagent event | Answer |
|---|---|---|---|
| `SessionStart` | (all) | `session_start` + git footprint | `additionalContext`: who else is working, and how to use the tools |
| `UserPromptSubmit` | (all) | `prompt` (first line becomes the task, unless sharing is off) | `additionalContext`: news from the team |
| `PreToolUse` | `Edit\|Write\|MultiEdit\|NotebookEdit\|Bash` | `pre_edit` for file writes, `tool_start` for Bash | `permissionDecision: deny` with a reason, `ask`, or `additionalContext` |
| `PostToolUse` | same | `post_edit`, or `tool_end` + git footprint after Bash | `additionalContext`: news from the team |
| `Stop` | (all) | `stop` + git footprint | nothing |
| `SessionEnd` | (all) | `session_end` + git footprint (timeout 5 s; Claude's default budget is 1.5 s) | nothing |

What matters:

- **Exec form.** Handlers are `{"command": "intagent", "args": ["hook", "claude-code"]}`, run without a shell, so
  shell profiles cannot pollute stdout. intagent prints nothing but the answer JSON.
- **Paths.** `Edit` and `Write` carry `tool_input.file_path`; `NotebookEdit` carries `tool_input.notebook_path`.
  Paths are absolute. Writes outside the worktree (Claude's scratchpad) are ignored.
- **A refusal reaches the model** as a failed tool call: `PreToolUse:Edit hook error: <reason>`. Models adapt to it.
- **`ask`** shows the person a permission prompt; in headless runs it acts as a refusal.
- **`additionalContext`** is shown to the model as a system reminder on all four events intagent answers.
- **Background task notifications** arrive as `UserPromptSubmit` prompts starting with `<task-notification>`; intagent
  does not treat them as a task description.
- **Start directory.** Claude reads `.claude/settings.json` only from the directory it starts in. Starting it in a
  subdirectory skips the project's hooks. `intagent init --user` also installs the hooks in `~/.claude/settings.json`;
  they do nothing in repositories without `.intagent.json`.
- **Trust.** In a folder you have not trusted, interactive sessions hold back hooks until you accept the trust dialog,
  and every session ignores the project's `permissions.allow`. Headless runs therefore need
  `--allowedTools 'mcp__intagent__*'` for the MCP tools (hooks run regardless). Claude asks each person once to
  approve the project's MCP server.
- **The MCP server** gets `CLAUDE_PROJECT_DIR` and `CLAUDE_CODE_SESSION_ID` in its environment, and its first message
  is a `server/discover` probe that intagent answers with "method not found", as Claude expects.
- **Failure is open.** A hook that crashes, times out or prints invalid JSON lets the tool run. intagent relies on this:
  when its server is unreachable, it prints nothing.

## Codex

Verified against the Codex CLI 0.160.0, with `codex exec` driven against a local mock of the model API.

**Installed by `intagent init`:** `.codex/hooks.json` and an `[mcp_servers.intagent]` table in
`.codex/config.toml`. **`intagent init --trust-codex`** also appends, to your `~/.codex/config.toml`, the project
trust entry and a `trusted_hash` for each intagent hook, computed with Codex's own recipe.

| Event | Matcher | intagent event |
|---|---|---|
| `SessionStart` (fires with the first prompt) | (all) | `session_start` + git footprint |
| `UserPromptSubmit` | (all) | `prompt` |
| `PreToolUse` | `^(apply_patch\|Bash)$` | `pre_edit` with the paths in the patch, or `tool_start` |
| `PostToolUse` | same | `post_edit`, or `tool_end` + git footprint after Bash |
| `Stop` | (all) | `stop` + git footprint |
| `SessionEnd` | (all) | `session_end` (timeout 3 s, Codex's maximum) |

What matters:

- **Two trust gates.** Codex ignores a project's hooks and MCP servers unless the project is trusted, and ignores each
  hook unless its hash is trusted in the user's config. `codex exec` does so silently.
- **Strict output.** Codex rejects unknown keys, an explicit `allow`, and `ask` from `PreToolUse`; a rejected answer is
  dropped entirely. intagent allows by printing nothing, refuses with `permissionDecision: deny`, and turns `ask` into a
  refusal that tells the agent to ask its person.
- **Edits are patches.** File edits arrive as `apply_patch` with the patch text in `tool_input.command`. intagent reads
  the `Add File`, `Update File`, `Delete File` and `Move to` lines. Patches piped through the shell
  (`apply_patch <<'EOF'`, optionally after `cd dir &&`) arrive as `Bash` and are parsed the same way.
- **MCP environment.** Codex passes the MCP server only a short list of environment variables. intagent's config lists
  `INTAGENT_URL`, `INTAGENT_TOKEN` and `INTAGENT_CONFIG` in `env_vars`; the token normally comes from the user
  config under `$HOME`, which Codex does pass. intagent's tools declare `destructiveHint: false` and
  `openWorldHint: false`, so Codex runs them without asking.
- **Linked worktrees.** Codex reads hook declarations from the main checkout's `.codex/`; `--trust-codex` trusts both
  roots.

## Cursor

Cursor reports edits after they happen (`afterFileEdit`), so it cannot be stopped before a write. intagent records
Cursor's edits and prompts, and Cursor's agent sees others' work through the MCP tools.

**Installed by `intagent init`:** `.cursor/hooks.json` (`beforeSubmitPrompt`, `afterFileEdit`, `stop`) and
`.cursor/mcp.json`.

## Any other agent

- `intagent watch` keeps a worktree on the board, reporting its git footprint on an interval and printing news.
- `intagent guard`, installed as a pre-commit hook with `intagent init --git-hook`, refuses commits that touch paths
  an active teammate holds exclusively.
- `intagent mcp` works with any MCP client that speaks stdio.
