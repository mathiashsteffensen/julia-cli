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
./bin/jarvis doctor   # check config, provider, reviewer, search key, browser, rg/bash
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

Other flags: `--model`, `--reasoning`, `--mode`, `--compatible-base-url`,
`--api-key-env`, `--timeout`, `--config`. Run `jarvis -h` for the full list.

| Key | Action |
| --- | --- |
| Enter / Alt+Enter | Send / new line |
| Esc | Cancel the running task |
| Ctrl+P or `/model` | Model picker: every model in one list |
| Ctrl+N | Switch to the next of your `enabled_models` |
| Shift+Tab or `/reasoning` | Change reasoning effort |
| Ctrl+O | Expand/collapse tool output |
| Ctrl+V | Attach the clipboard's image (a screenshot, or an image file copied in Finder) |
| Drop image files | Attach them instead of pasting their paths |
| Drag | Copy the selection (without borders or padding) |
| Click a code block | Copy its code |
| Double-click | Copy a whole message, a tool's command, or its output |
| `/copy`, `/copy code` | Copy the last reply, or its last code block |
| `/clear`, `/help`, `/quit` | New conversation, shortcuts, exit |

During an approval prompt, `y`/Enter allows the action once (a Browser origin, for
the rest of the turn); any other key denies it.

Each attached image puts an `[Image #N]` token in your message; delete the token
to drop the image. Images are scaled to at most 2000 px on the long side and sent
to every provider, Codex included. Ctrl+V falls back to an ordinary text paste
when the clipboard holds no image. On Linux it needs `wl-paste` or `xclip`. In Ghostty, sent
images are drawn inline as thumbnails; other terminals show a caption instead.

JARVIS handles selection itself, because mouse reporting is on and rows carry
borders and the sidebar. Copies go to the system clipboard, or through the
terminal (OSC 52) over SSH.

## Providers

| Provider | Backend | API key env |
| --- | --- | --- |
| `codex` (default) | Local Codex app-server, your ChatGPT sign-in | — |
| `openai` | Responses API | `OPENAI_API_KEY` |
| `anthropic` | Messages API | `ANTHROPIC_API_KEY` |
| `google` | Gemini API | `GEMINI_API_KEY` |
| `compatible` | OpenAI-compatible chat completions (OpenRouter, Ollama, …) | `OPENAI_API_KEY` or `--api-key-env` |

Models are named `provider/<model-id>`, in the config and in `--model`. A bare
`codex` is the model your Codex is configured with; every other provider needs a
model ID. Reasoning effort is only supported on Codex and OpenRouter. Endpoints
must be HTTPS unless they are on loopback.

```sh
jarvis --model anthropic/MODEL_ID
jarvis --model compatible/vendor/MODEL_ID \
  --compatible-base-url https://openrouter.ai/api/v1 --api-key-env OPENROUTER_API_KEY
```

## Configuration

`jarvis config init` writes `~/.config/jarvis/config.json` (respects
`$XDG_CONFIG_HOME`). Flags override it. Repo-local config is never loaded, and
keys are only referenced by env var name.

```json
{
  "model": "codex/gpt-6-astra",
  "reasoning": "medium",
  "mode": "auto",
  "max_steps": 256,
  "context_tokens": 256000,
  "timeout": "4h",
  "compatible_base_url": "https://openrouter.ai/api/v1",
  "api_key_env": "OPENROUTER_API_KEY",
  "enabled_models": ["codex/gpt-6-astra", {"id": "compatible/vendor/model-b", "reasoning": "high"}],
  "skill_paths": ["~/.codex/skills/coding-assistance-style/SKILL.md"],
  "browser": "/Applications/Chromium.app"
}
```

- `enabled_models` come first in the model picker, ahead of the rest of Codex's catalog, and Ctrl+N cycles through them.
- `model` and each `enabled_models` entry are `provider/<model-id>`, or `{"id": "provider/<model-id>", "reasoning": "<effort>"}` for a model that starts at that effort when you launch with it or switch to it.
- `context_tokens` is the assumed window for every model. API providers compact the history at 85%.
- `skill_paths` are extra instruction files added to every turn. If you omit it, JARVIS loads `coding-assistance-style/SKILL.md` from `$CODEX_HOME/skills` or `~/.agents/skills`. `[]` disables skills.
- The last reasoning effort you picked for each provider is remembered in `~/.local/state/jarvis/preferences.json`. It applies at launch unless the model has its own `reasoning`, and `--reasoning` beats both.
- `browser` is the Chrome the Browser tool launches (an executable or `.app`); `JARVIS_BROWSER` overrides it. Without either, JARVIS looks for Chrome, Chromium, Chrome for Testing, Brave or Edge in `/Applications`, then in Playwright's and Puppeteer's caches. `npx playwright install chromium` provides one.

Sessions are stored in `~/.local/state/jarvis/sessions` with 0600 permissions.
They contain transcripts and command output.

## Tools

`Read`, `ViewImage`, `List`, `Grep`, `Edit`, `MultiEdit`, `Write`, `Bash`,
`Terminal`, `Browser`, `WebSearch`, `Subagent` (up to 3 parallel child tasks), and
`AskUserQuestion` (interactive only). File tools are confined to the workspace,
refuse symlinks and credential paths, and cap output at 32 KiB.

`ViewImage` shows the model an image from the workspace or `$JARVIS_SCRATCH`,
such as a screenshot it rendered, scaled to at most 2000 px. `Terminal` runs a
program on a virtual terminal under the Bash sandbox, so the agent can drive a TUI,
REPL or dev server with keys, text and mouse, and read the screen as text or, when
the look matters, as a rendered screenshot. Up to 4 run at once. At the end of the
turn each is hung up, as closing a terminal window does, and everything still on it
is killed. Ctrl+O shows a terminal's screen in its card, and screenshots appear as
thumbnails in Ghostty.

`Browser` opens pages in JARVIS's own headless Chrome, so the agent can check a
web UI as a person would: start the dev server in a loopback terminal, then
navigate, click, type, press keys, select, upload, scroll, drag, wait, evaluate
JavaScript, read the console and resize or switch to dark mode. Every action
answers, once the page has settled (the requests it started get up to 1.5 s), with
the page's accessibility snapshot, or only the lines that changed, plus notes on
popups, dialogs, blocked requests and console errors; a long page is shown around
what is in view, and a screenshot shows the look. A click is refused, as a person's
would miss, when a backdrop or banner covers its target or a container cuts it off.
Chrome starts on the first navigate with a throwaway profile, talks to JARVIS over
a pipe rather than a debugging port, gives each subagent its own context, and is
killed with every process it started, profile and crash dumps and all, when the
turn ends, or by a watchdog should JARVIS die first. Ctrl+O shows a page's snapshot
in its card, its changes lit as a diff.

`WebSearch` uses Firecrawl. Set `FIRECRAWL_API_KEY`, or let the first search open
a browser authorization flow that saves a key to `~/.config/jarvis/firecrawl.json`.

## Permissions and sandbox

| Mode | Behavior |
| --- | --- |
| `auto` (default) | Reads, workspace edits, offline or loopback sandboxed Bash and terminals, and the Browser on loopback pages and workspace files run freely. Bash with network, escalated Bash, a networked terminal's start and every input to it, web searches, each new Browser origin, and text, keys, uploads and scripts sent to a page beyond loopback go to the reviewer model. |
| `ask` | You approve every mutation, command, terminal start and input, search, and Browser action on a page (navigating, clicking, typing, scripts); looking at a page needs no approval. |
| `read-only` | Only `Read`, `ViewImage`, `Grep`, and `List`. |

**Sandbox.** Bash runs under a deny-by-default Seatbelt profile. It can write
only to the workspace (never `.git`), a temp dir, isolated Go caches, and
`$JARVIS_SCRATCH`. It has no network and no access to home credentials, and
credentials are stripped from its environment. A `Terminal` program gets the same
profile plus its own terminal, through `/dev/tty` and the descriptors it inherits;
no terminal can be opened by its device name, which macOS reuses. The agent can
request `network: true` for a single call, or for one terminal's lifetime, or
`loopback: true`, which lets it listen on localhost and connect to the servers it,
or the agent's loopback terminals, start there: dev servers and tests that start
local servers. Services already running on the machine (a database, your own dev
server, JARVIS's browser proxy) and Unix sockets stay closed to it; they need
`network: true`. Auto mode runs loopback commands without review, since they reach
only what the agent itself started.
Seatbelt cannot tell a wildcard listener from a loopback one, so a server bound to
every interface still accepts the local network, as it would if you ran it; bind
127.0.0.1 to avoid that. A Bash call can
also request `sandbox_permissions: "require_escalated"` with a justification, which
runs that exact command on the host after approval; terminals never leave the
sandbox. If the sandbox can't be set up, the command fails rather than running
unsandboxed.

**Browser network policy.** Chrome sandboxes its own renderers and runs outside
Seatbelt, so its reach is decided per request instead, at two layers that both
refuse by default. Every request a page, frame or worker makes (documents and
redirects, subresources, fetch, workers) is paused until the policy rules on it, and
every connection beyond loopback goes through a JARVIS proxy that opens only
approved hosts, which also stops WebSockets and preconnects. Without asking, a page
reaches loopback servers, files in the workspace and `$JARVIS_SCRATCH` (served by
JARVIS without following symlinks, never credential or `.git` paths), and `data:`
and `blob:` URLs. Any other origin, a CDN or font host included, stays blocked until
approved for the rest of the turn (`network: true` on navigate, or the `allow`
action), once per origin, by the reviewer or by you. The trade-off: a page or an
evaluated script can send what it reads to any origin it reaches, so each one is
judged once as a destination, and what the agent itself types, uploads or evaluates
on a page beyond loopback (or in a popup such a page opened) is reviewed again. A
page or frame from beyond loopback cannot use local servers except by navigating to
them: the policy judges each frame's requests by its own document, and Chrome counts
what arrives through the proxy as the public internet, so its Local Network Access
checks refuse such a document's WebSockets to loopback too. Only Chrome may use the
proxy. An approved name must resolve to a public address, so DNS rebinding cannot
turn it into a LAN or localhost client. Downloads are saved where the sandbox cannot
reach and copied into `$JARVIS_SCRATCH/downloads` without following symlinks. Page
text is untrusted: the agent treats it as data, never as instructions.

**Reviewer.** `reviewer_model` in the config picks it, as `provider/<model-id>`.
Unset, it is `codex/gpt-6-luna`: Codex with no reasoning, running in a
read-only, no-network process. `compatible/<model-id>`, such as
`compatible/typesafe/jev-1.13`, reviews instead through `compatible_base_url`'s
Decisions API. Approval then requires ≥50% task relevance and >70% on each harm
check. The reviewer sees the proposed action and your messages, but nothing else
from the agent. A denial goes back to the
agent with the reason. After 3 denials in a row (or 20 in one turn), or when the
reviewer fails, you are asked instead. In headless mode the action is denied.

## Development

```sh
make check                                                        # go vet + go test -race
JARVIS_SANDBOX_TEST=1 go test ./internal/tools -run SandboxIntegration -v
JARVIS_BROWSER=/path/to/chrome go test ./internal/browser ./internal/tools   # against a particular Chrome
JARVIS_CODEX_REVIEW_POLICY_LIVE=1 go test ./internal/provider -run TestCodexLiveReviewInspectionPolicy -count=1 -v
JARVIS_JEV_REVIEW_POLICY_LIVE=1  go test ./internal/provider -run TestJevLiveReviewDevelopmentPolicy -count=1 -v
```

The default test suite is hermetic. Browser tests drive the Chrome JARVIS finds,
against loopback test servers only, and skip without one. The live review suites
send only synthetic proposals and never execute them.

| Package | Role |
| --- | --- |
| `cmd/jarvis` | CLI entry, flags, subcommands, wiring |
| `internal/core` | Shared types, `Backend` interface, system prompt (`system.md`) |
| `internal/provider` | Codex app-server and Fantasy adapters, compaction, reviewers |
| `internal/tools` | Tool execution, sandbox, permissions, browser network policy, web search, subagents |
| `internal/browser`, `internal/terminal` | Headless Chrome over the DevTools protocol; virtual terminals |
| `internal/ui` | Bubble Tea TUI |
| `internal/session`, `internal/config`, `internal/instructions` | Persistence, settings, skill loading |
