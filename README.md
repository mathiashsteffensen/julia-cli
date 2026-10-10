# JARVIS

A terminal coding agent written in Go (Bubble Tea / Fantasy). It runs on your
Codex subscription or any of several API providers, executes Bash in a macOS
Seatbelt sandbox, and sends risky actions to a separate reviewer model before
they run.

![JARVIS welcome screen](docs/welcome.png)

## Principle: it should be gorgeous

A core principle of this harness is that it should be gorgeous. You spend hours
reading what it shows you, so how it looks is part of how well it works. Every
surface is designed: a code block gets a card, a selection is lit, and a status
line says only what matters. A change that works but looks careless is not done.

## Install

Requires Go 1.27+, `rg`, and macOS (sandboxed Bash is macOS-only for now). For
the default provider, install and sign in to the Codex CLI. JARVIS also finds the
copy bundled with the ChatGPT macOS app.

```sh
make build            # → bin/jarvis
./bin/jarvis doctor   # check config, provider, reviewer, search key, rg/bash
./bin/jarvis
```

Or `go install ./cmd/jarvis` to put `jarvis` in `$GOBIN`.

## Usage

```sh
jarvis                         # TUI in the current directory
jarvis --dir ~/code/project    # choose the workspace
jarvis --continue              # resume this workspace's latest session
jarvis --resume SESSION_ID     # resume a specific session
jarvis --prompt "fix the build" [--json]   # headless, one task
jarvis --demo                  # offline UI preview
jarvis sessions | login | doctor | config init
```

Other flags: `--provider`, `--model`, `--reasoning`, `--mode`, `--base-url`,
`--api-key-env`, `--timeout`, `--config`. Run `jarvis -h` for the full list.

| Key | Action |
| --- | --- |
| Enter / Alt+Enter | Send / new line |
| Esc | Cancel the running task |
| Ctrl+P or `/model` | Model picker |
| Shift+Tab or `/reasoning` | Change reasoning effort |
| Ctrl+O | Expand/collapse tool output |
| Drag | Copy the selection (without borders or padding) |
| Click a code block | Copy its code |
| Double-click | Copy a whole message, a tool's command, or its output |
| `/copy`, `/copy code` | Copy the last reply, or its last code block |
| `/clear`, `/help`, `/quit` | New conversation, shortcuts, exit |

During an approval prompt, `y`/Enter allows the action once; any other key denies it.

JARVIS handles selection itself, because mouse reporting is on and rows carry
borders and the sidebar. Copies go to the system clipboard, or through the
terminal (OSC 52) over SSH.

## Providers

| `provider` | Backend | API key env |
| --- | --- | --- |
| `codex` (default) | Local Codex app-server, your ChatGPT sign-in | — |
| `openai` | Responses API | `OPENAI_API_KEY` |
| `anthropic` | Messages API | `ANTHROPIC_API_KEY` |
| `google` | Gemini API | `GEMINI_API_KEY` |
| `compatible` | OpenAI-compatible chat completions (OpenRouter, Ollama, …) | `OPENAI_API_KEY` or `--api-key-env` |

API providers need an explicit `--model`. Reasoning effort is only supported on
Codex and OpenRouter. Endpoints must be HTTPS unless they are on loopback.

```sh
jarvis --provider anthropic --model MODEL_ID
jarvis --provider compatible --model MODEL_ID \
  --base-url https://openrouter.ai/api/v1 --api-key-env OPENROUTER_API_KEY
```

## Configuration

`jarvis config init` writes `~/.config/jarvis/config.json` (respects
`$XDG_CONFIG_HOME`). Flags override it. Repo-local config is never loaded, and
keys are only referenced by env var name.

```json
{
  "provider": "codex",
  "model": "gpt-6-astra",
  "reasoning": "medium",
  "mode": "auto",
  "max_steps": 256,
  "context_tokens": 256000,
  "timeout": "4h",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key_env": "OPENROUTER_API_KEY",
  "compatible_models": ["vendor/model-a", "vendor/model-b"],
  "skill_paths": ["~/.codex/skills/coding-assistance-style/SKILL.md"]
}
```

- `compatible_models` populates the second tab of the model picker. Codex models come from Codex itself.
- `context_tokens` is the assumed window for every model. API providers compact the history at 85%.
- `skill_paths` are extra instruction files added to every turn. If you omit it, JARVIS loads `coding-assistance-style/SKILL.md` from `$CODEX_HOME/skills` or `~/.agents/skills`. `[]` disables skills.
- The last reasoning effort you picked for each provider is remembered in `~/.local/state/jarvis/preferences.json`.

Sessions are stored in `~/.local/state/jarvis/sessions` with 0600 permissions.
They contain transcripts and command output.

## Tools

`Read`, `Write`, `Edit`, `MultiEdit`, `Grep`, `List`, `Bash`, `WebSearch`,
`Subagent` (up to 3 parallel child tasks), and `AskUserQuestion` (interactive only).
File tools are confined to the workspace, refuse symlinks and credential paths,
and cap output at 32 KiB.

`WebSearch` uses Firecrawl. Set `FIRECRAWL_API_KEY`, or let the first search open
a browser authorization flow that saves a key to `~/.config/jarvis/firecrawl.json`.

## Permissions and sandbox

| Mode | Behavior |
| --- | --- |
| `auto` (default) | Reads, workspace edits, and offline sandboxed Bash run freely. Bash with network, escalated Bash, and web searches go to the reviewer model. |
| `ask` | You approve every mutation, command, and search. |
| `read-only` | Only `Read`, `Grep`, and `List`. |

**Sandbox.** Bash runs under a deny-by-default Seatbelt profile. It can write
only to the workspace (never `.git`), a temp dir, isolated Go caches, and
`$JARVIS_SCRATCH`. It has no network and no access to home credentials, and
credentials are stripped from its environment. The agent can request
`network: true` for a single call. It can also request
`sandbox_permissions: "require_escalated"` with a justification, which runs that
exact command on the host after approval. If the sandbox can't be set up, the
command fails rather than running unsandboxed.

**Reviewer.** By default this is Codex `gpt-6-luna` with no reasoning, running in
a read-only, no-network process. If `compatible_models` lists `typesafe/jev-1.13`,
that model reviews instead via `base_url`. In that case approval requires ≥50%
task relevance and >70% on each harm check. The reviewer sees the proposed action
and your messages, but nothing else from the agent. A denial goes back to the
agent with the reason. After 3 denials in a row (or 20 in one turn), or when the
reviewer fails, you are asked instead. In headless mode the action is denied.

## Development

```sh
make check                                                        # go vet + go test -race
JARVIS_SANDBOX_TEST=1 go test ./internal/tools -run SandboxIntegration -v
JARVIS_CODEX_REVIEW_POLICY_LIVE=1 go test ./internal/provider -run TestCodexLiveReviewInspectionPolicy -count=1 -v
JARVIS_JEV_REVIEW_POLICY_LIVE=1  go test ./internal/provider -run TestJevLiveReviewDevelopmentPolicy -count=1 -v
```

The default test suite is hermetic. The live review suites send only synthetic
proposals and never execute them.

| Package | Role |
| --- | --- |
| `cmd/jarvis` | CLI entry, flags, subcommands, wiring |
| `internal/core` | Shared types, `Backend` interface, system prompt (`system.md`) |
| `internal/provider` | Codex app-server and Fantasy adapters, compaction, reviewers |
| `internal/tools` | Tool execution, sandbox, permissions, web search, subagents |
| `internal/ui` | Bubble Tea TUI |
| `internal/session`, `internal/config`, `internal/instructions` | Persistence, settings, skill loading |
