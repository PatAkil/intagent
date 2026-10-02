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

- **One command, in shell form.** Every handler is
  `command -v intagent >/dev/null && intagent hook || true`, without `args`. Shell form, because Cursor and Copilot
  CLI run these hooks too and Cursor keeps only the `command` string. Guarded, because the file is committed:
  for a teammate without intagent the hook does nothing, where a missing binary would otherwise make Copilot CLI
  refuse every edit. A non-interactive `sh -c` reads no profile, and intagent prints nothing but the answer JSON.
  `intagent init` replaces the exec form (`"args": ["hook", "claude-code"]`) that earlier versions wrote.
- **intagent never exits 2.** Exit 2 is a refusal in Claude Code, Cursor and Copilot CLI, so a usage error exits 1
  and the hook itself always exits 0.
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

Verified against the agent runtime of `@cursor/sdk` 1.0.35, which runs Cursor's hooks; the editor itself was not
driven.

**Installed by `intagent init`:** `.cursor/hooks.json` and `.cursor/mcp.json`. Each hook runs the same command as
Claude Code's, with a 10 s timeout.

| Event | Matcher | intagent event | Answer |
|---|---|---|---|
| `sessionStart` | (all) | `session_start` + git footprint | `additional_context` |
| `beforeSubmitPrompt` | (all) | `prompt` | `continue: true` and `additional_context` |
| `preToolUse` | `^(Write\|Delete\|Shell)$` | `pre_edit` for `Write` and `Delete` (`tool_input.file_path`), `tool_start` for `Shell` | `permission: deny` with `user_message` and `agent_message`, or `permission: allow`; `additional_context` |
| `postToolUse` | same | `post_edit`, or `tool_end` + git footprint after `Shell` | `additional_context` |
| `stop` | (all) | `stop` + git footprint | `{}` |
| `sessionEnd` | (all) | `session_end` + git footprint | `{}` |

What matters:

- **Edits are tool calls.** Every agent edit reaches `preToolUse` as tool `Write` (deletions as `Delete`), so Cursor
  can be refused before the write. Matchers are unanchored regular expressions; intagent anchors its own.
- **Always valid JSON.** Cursor treats invalid JSON from a permission step as a refusal, so every answer is JSON and
  an allow is spelled out. Cursor ignores `ask` on `preToolUse` for local tools; intagent turns it into a refusal
  that tells the agent to ask its person.
- **One unknown event drops the file.** Cursor rejects a whole hooks file that names an event it does not know. A
  Cursor too old for `preToolUse` and `sessionStart` therefore runs none of intagent's hooks there, and its agent
  sees the team only through the MCP tools. intagent still parses the older `afterFileEdit`,
  `beforeShellExecution` and `afterShellExecution` events if you wire them by hand.
- **Cursor runs Claude Code's hooks too.** It imports the hooks in `.claude/settings.json`, keeps only their
  `command`, and sends them Cursor's payloads. intagent recognises a Cursor payload whichever file the hook came
  from (`cursor_version`, or a lower-case event name with a `conversation_id`), and writes the identical command in
  both files: Cursor drops an imported hook whose command equals one of its own for the same event.
- **Sessions** are `conversation_id`; paths resolve against `workspace_roots[0]`.
- **Windows.** Cursor runs hooks in PowerShell there, which cannot run the guarded command; Cursor carries on without
  intagent (it fails open). Untested.

## GitHub Copilot CLI

Verified against GitHub Copilot CLI 1.0.91.

**Installed by `intagent init`:** nothing of its own. Copilot CLI runs the hooks in `.claude/settings.json` and the
MCP server in `.mcp.json` (in a folder you have trusted).

What matters:

- **Claude's dialect, Copilot's arguments.** Payloads use Claude Code's event and tool names (`create` arrives as
  `Write`, `edit` as `Edit`) but keep Copilot's argument names: `tool_input.path`, `file_text`. Relative paths arrive
  as the model wrote them and resolve against `cwd`. intagent recognises Copilot by `COPILOT_CLI=1` in the
  environment.
- **Answers.** intagent prints both Copilot's top-level `permissionDecision` and Claude's nested form; the model
  reads `Denied by preToolUse hook: <reason>`. `additionalContext` reaches the model on session start, prompts and
  around tool calls.
- **Failure is closed.** A pre-tool hook that exits 1, or whose binary is missing, refuses the tool call. intagent's
  hook always exits 0, and the guard in the command covers teammates who have not installed it.

## Any other agent

- `intagent watch` keeps a worktree on the board, reporting its git footprint on an interval and printing news.
- `intagent guard`, installed as a pre-commit hook with `intagent init --git-hook`, refuses commits that touch paths
  an active teammate holds exclusively.
- `intagent mcp` works with any MCP client that speaks stdio.
