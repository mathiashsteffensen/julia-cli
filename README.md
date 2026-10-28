# JULIA

**J**ust **U**sing **L**anguage **I**ntelligently, **A**pparently

A terminal coding agent written in Go (Bubble Tea / Fantasy). It runs on your
Codex or Claude subscription or any compatible API providers. It executes Bash in a macOS
Seatbelt sandbox, and sends risky actions to a separate reviewer model before
they run.

![JULIA welcome screen](docs/welcome.png)

Note: This harness is designed to be opinionated. Underlying models are highly configurable but tools, workflows and the TUI are fixed. If you do not like it, this harness is not for you and I would highly recommend you give something like [Pi](https://pi.dev) a try.

Also note: This harness is largely slop. The codebase is a mess and should not be viewed by the faint of heart. However, it works and is a joy to use. The sandboxing limits and auto-approvals are however very well-tested. Running this harness in auto mode should be considered as safe as using auto mode in a harness like Codex or Claude Code.

## Principle: it should be gorgeous

A core principle of this harness is that it should be gorgeous. You spend hours
reading what it shows you, so how it looks is part of how well it works. Every
interface is designed: a code block gets a card, a selection is lit, and a status
line says only what matters. A change that works but looks careless is not done.

## Install

Requires Go 1.27+, `rg`, and macOS (sandboxed Bash is macOS-only for now, do not run this on your dev machine if you use linux). For
the default provider, install and sign in to the Codex or Claude CLI. JULIA also finds the copy bundled with the ChatGPT macOS app.

```sh
make build            # → bin/julia
./bin/julia doctor   # check config, provider, reviewer, subagent roles, search key, memory, browser, rg/bash
./bin/julia
```

Or `go install ./cmd/julia` to put `julia` in `$GOBIN`.

## Usage

```sh
julia                         # TUI in the current directory
julia --dir ~/code/project    # choose the workspace
julia --continue              # resume this workspace's latest session
julia --resume SESSION_ID     # resume a specific session
julia --prompt "fix the build" [--json]   # headless, one task
julia --demo                  # offline UI preview
julia --profile work          # run on ~/.config/julia/config.work.json
julia sessions | login | doctor | config init
julia memory [show NAME | forget NAME | confirm NAME | pin NAME | unpin NAME | stats [-days N] [-all] | consolidate | path | workspaces]
```

Other flags: `--model`, `--reasoning`, `--mode`, `--memory off|read|write`,
`--timeout`, `--config`, `--profile`. Run `julia -h`
for the full list.

| Key                           | Action                                                                                                                 |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| Enter / Alt+Enter             | Send / new line                                                                                                        |
| Enter while JULIA works       | Queue the message; it is sent when the turn settles (a turn that fails or is interrupted puts it back in the composer) |
| Ctrl+S or the steer button    | Steer: hand the queued message, or the draft, to the turn under way, which reads it after its current step             |
| ↑ in an empty composer        | Take the last queued message back to edit                                                                              |
| Esc                           | Cancel the running task                                                                                                |
| Ctrl+P or `/model`            | Model picker: every model in one list                                                                                  |
| Ctrl+N                        | Switch to the next of your `enabled_models`                                                                            |
| Shift+Tab or `/reasoning`     | Change reasoning effort                                                                                                |
| `/profile` or `/profile NAME` | Switch configuration profile                                                                                           |
| Ctrl+O                        | Expand/collapse tool output, full subagent tasks, and the arguments of Slack, Linear and Mail calls                    |
| Ctrl+F                        | The fleet: every subagent of the session, with its task, tool calls and report                                         |
| Ctrl+V                        | Attach the clipboard's image (a screenshot, or an image file copied in Finder)                                         |
| Drop image files              | Attach them instead of pasting their paths                                                                             |
| Drag                          | Copy the selection (without borders or padding)                                                                        |
| Click a code block            | Copy its code                                                                                                          |
| Double-click                  | Copy a whole message, a tool's command, or its output                                                                  |
| `/copy`, `/copy code`         | Copy the last reply, or its last code block                                                                            |
| `/review`, `/review WHAT`     | Review your uncommitted changes, or what you name (a commit, a path, a PR), and triage the findings                    |
| `/memory`                     | What JULIA remembers here · `/memory NAME` shows a note · `/memory forget NAME` deletes it                             |
| `/clear`, `/help`, `/quit`    | New conversation, shortcuts, exit                                                                                      |

During an approval prompt, `y`/Enter allows the action once (a Browser origin, for
the rest of the turn); any other key denies it.

## Providers

| Provider          | Backend                                                               | API key env                |
| ----------------- | --------------------------------------------------------------------- | -------------------------- |
| `codex` (default) | Local Codex app-server, your ChatGPT sign-in                          | —                          |
| `claude`          | Local Claude Code CLI, your Claude sign-in                            | —                          |
| `openai`          | Responses API                                                         | `OPENAI_API_KEY`           |
| `anthropic`       | Messages API                                                          | `ANTHROPIC_API_KEY`        |
| `google`          | Gemini API                                                            | `GEMINI_API_KEY`           |
| your `providers`  | OpenAI-compatible chat completions (OpenRouter, LM Studio, Ollama, …) | its `api_key_env`, or none |

```json
{
  "providers": [
    {
      "id": "openrouter",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_API_KEY"
    },
    { "id": "local", "base_url": "http://localhost:1234/v1" }
  ]
}
```

```sh
julia --model anthropic/MODEL_ID
julia --model openrouter/vendor/MODEL_ID
julia --model local/MODEL_ID
```

An `id` is lowercase letters, digits, `-` or `_`, and cannot be a built-in
provider's name. A provider's key is read only from its own `api_key_env` and is
sent only to its own `base_url`; one without `api_key_env` is called keyless.

## Configuration

`julia config init` writes `~/.config/julia/config.json` (respects
`$XDG_CONFIG_HOME`). Flags override it. Repo-local config is never loaded, and
keys are only referenced by env var name.

Full reference:

```json
{
  "model": "codex/gpt-6-astra",
  "reasoning": "medium",
  "mode": "auto",
  "max_steps": 256,
  "context_tokens": 256000,
  "timeout": "4h",
  "providers": [
    {
      "id": "openrouter",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_API_KEY"
    }
  ],
  "enabled_models": [
    "codex/gpt-6-astra",
    {
      "id": "openrouter/vendor/model-b",
      "reasoning": "high",
      "context_window": 1000000
    }
  ],
  "workspace_instructions": ["AGENTS.md"],
  "browser": "/Applications/Chromium.app",
  "subagents": {
    "scout": "openrouter/vendor/fast-model",
    "reviewer": { "id": "openrouter/vendor/model-b", "reasoning": "high" },
    "oracle": "codex/gpt-6-astra"
  },
  "memory": {
    "reflection": "on",
    "model": "codex/gpt-6-astra",
    "index_bytes": 16384
  }
}
```

- `enabled_models` come first in the model picker, ahead of the rest of Codex's and Claude Code's catalogs, and Ctrl+N cycles through them.
- `model` and each `enabled_models` entry are `provider/<model-id>`, or `{"id": "provider/<model-id>", "reasoning": "<effort>", "context_window": <tokens>}` for a model that starts at that effort when you launch with it or switch to it, and has a context window of its own. Both keys are optional.
- `context_tokens` is the assumed window for every model without a `context_window` of its own. API providers compact the history at 85% of the model's window, and the sidebar's context gauge measures against it.
- `workspace_instructions` names files at the workspace root, such as `AGENTS.md` or `CLAUDE.md`, whose contents join every turn as the repository's own context, labelled as such and without authority over the task. Omitted, nothing is read from a repository: a cloned project's files are not trusted by default. A missing file is skipped; a symlink or a file over 64 KiB is a warning.
- The last reasoning effort you picked for each provider is remembered in `~/.local/state/julia/preferences.json`. It applies at launch unless the model has its own `reasoning`, and `--reasoning` beats both.
- Every model call is timed, and `~/.local/state/julia/throughput.json` keeps each model's last week of calls. The model picker (`ctrl+p`) shows each model's output tokens per second over them, with how long ago it was last used once that is more than a week. A model's old calls are dropped only when it is used again, so an idle model keeps its figures.
- `subagents` assigns a model to each [subagent role](#subagent-roles) by name, as `provider/<model-id>` or `{"id": ..., "reasoning": ...}`. A role without one runs on the delegating agent's model. An assigned model starts at its entry's effort, else at the parent's when it is the parent's model, else at the one `model` or `enabled_models` gives it.
- `memory` keeps notes between sessions (see [Memory](#memory)): `reflection` is `on` (the default) or `off`, `model` runs the reflection step on another model, as `provider/<model-id>` or `{"id": ..., "reasoning": ...}`, and `index_bytes` caps what the index of notes may take of the instructions (4096 to 65536; default 16 KiB). The reflection step runs at low effort unless `model` gives its own.
- `browser` is the Chrome the Browser tool launches (an executable or `.app`); `JULIA_BROWSER` overrides it. Without either, JULIA looks for Chrome, Chromium, Chrome for Testing, Brave or Edge in `/Applications`, then in Playwright's and Puppeteer's caches. `npx playwright install chromium` provides one.

### Skills

JULIA offers every [Agent Skill](https://agentskills.io/specification) in
`~/.agents/skills`: each folder there whose `SKILL.md` frontmatter gives a
`name` matching the folder and a `description`. Every turn's instructions list
each skill's name and description, and the model loads a skill's instructions,
or a file beside them, with the read-only Skill tool when a task calls for it.
A symlinked skill folder is followed; nothing outside a skill's folder can be
read through the tool. Skills marked `disable-model-invocation: true` are left
out, and a malformed one is left out with a warning. `julia doctor` lists what
it finds. The old `skill_paths` key is ignored.

### Profiles

Each `config.<name>.json` beside `config.json` is a profile named `<name>`;
`config.json` itself is the `default` profile. `julia --profile work` launches
on `config.work.json`, and `julia --profile work config init` creates it.
Profile names are letters, digits, `-` and `_`.

In the TUI, `/profile` lists the profiles with each one's model and mode, and
`/profile NAME` switches directly. The switch applies from the next turn: the
session moves to the profile's `model`, as choosing it with `/model` would
(a fresh conversation when the provider or an API model changes, the old one
saved), and everything else in the profile — mode, providers,
`enabled_models`, subagents, memory, timeout — replaces the old profile's.
`--mode` and `--timeout` given at launch hold in every profile. Switching to
the active profile reloads its file. A profile that does not load, or whose
provider has no key, is refused and nothing changes.

Sessions are stored in `~/.local/state/julia/sessions` with 0600 permissions.
They contain transcripts and command output.

## Tools

`Read`, `ViewImage`, `List`, `Grep`, `Edit`, `MultiEdit`, `Write`, `Bash`,
`Terminal`, `Browser`, `WebSearch`, `WebFetch`, `Slack` (when a bot token is set),
`Linear` (when an access token is saved), `Mail` (when a mailbox's credentials
are saved), `Subagent` (up to 3 parallel child tasks), `Memory` (see [Memory](#memory)),
`Skill` (see [Skills](#skills)), and `AskUserQuestion` (interactive only).
File tools are confined to the workspace, refuse symlinks and credential paths, and cap output at 32 KiB.

`ViewImage` shows the model an image from the workspace or `$JULIA_SCRATCH`,
such as a screenshot it rendered, scaled to at most 2000 px.

`Terminal` runs a program on a virtual terminal under the Bash sandbox, so the agent can drive a TUI.

`Browser` opens pages in JULIA's own headless Chrome, so the agent can check a
web UI as a person would: start the dev server in a loopback terminal, then
navigate, click, type, press keys, select, upload, scroll, drag, wait, evaluate
JavaScript, read the console and resize or switch to dark mode.

`WebSearch` and `WebFetch` use Firecrawl: search returns titles, URLs and
snippets, and fetch returns a public page's main content as Markdown, 24 KiB at
a time. Set `FIRECRAWL_API_KEY`, or let the first call open a browser
authorization flow that saves a key to `~/.config/julia/firecrawl.json`.

`Slack` appears when `SLACK_BOT_TOKEN` holds a bot token: post a message, react
to one, read a thread or channel, or list the channels the bot is in. Posting
and reacting are reviewed in auto mode and approved in ask mode; reading is
free. `SLACK_OWNER_USER_ID` tells the agent whose bot it is, so it can name the
owner in a thread and message them directly. The bot needs the scopes
`chat:write`, `reactions:write`, `channels:read`, `groups:read`,
`channels:history`, `groups:history`, `im:history` and `im:write`.

`Linear` appears when `~/.config/julia/linear.json` holds `{"api_key": "…"}`
or `LINEAR_ACCESS_TOKEN` is set.

`Mail` appears when `~/.config/julia/mail.json` holds a mailbox's IMAP
credentials. It is made for [Proton Mail Bridge](https://proton.me/mail/bridge),
which decrypts your account on this machine and serves it over IMAP on `127.0.0.1:1143` with a password of its own
(a paid plan is needed): save `{"username": "you@proton.me", "password": "<the Bridge password>"}`,
and JULIA connects with STARTTLS.

### Subagent roles

Each task given to `Subagent` names a role, which adds instructions for that kind
of work, can limit the child to some tools, and can run it on its own model (see
`subagents` above). The built-in roles are Markdown files in [`agents/`](agents):

| Role         | For                                                                                    | Tools                                          |
| ------------ | -------------------------------------------------------------------------------------- | ---------------------------------------------- |
| `scout`      | Fast reconnaissance: compressed context, with paths and line ranges, for another agent | Read, List, Grep, ViewImage, Bash              |
| `researcher` | Web research: a sourced brief that keeps evidence apart from inference                 | WebSearch, WebFetch, Browser, Read, List, Grep |
| `worker`     | Implementation of a well-defined task or approved plan, validated                      | all                                            |
| `reviewer`   | Evidence-backed review of a diff, plan or area of the code, by severity                | Read, List, Grep, ViewImage, Bash              |
| `oracle`     | A second opinion on a decision: drift, contradictions, hidden assumptions              | Read, List, Grep, ViewImage, Bash              |
| `delegate`   | Anything that fits no other role                                                       | all                                            |

Ctrl+F opens the fleet, a window over the conversation that lists every
subagent the session has run down its left side.

A role's tools are the only ones its children are offered or can run. Bash can
still write the workspace, so the read-only roles keep to inspection by
instruction.

To change a role or add one, put a file of the same shape in an
`agents/` directory beside your config file (`~/.config/julia/agents/` by
default): a file named like a built-in role replaces it. Roles are never read
from a repository.

```markdown
---
name: auditor
description: Checks that a change's claims are backed by tests. One line; the delegating agent picks roles by it.
tools: Read, Grep, Bash
---

The instructions a child of this role adds to its own.
```

`name` must match the file name. `tools` is optional; without it a child has
every tool a subagent can have (all but `Subagent` and `AskUserQuestion`).
`julia doctor` lists the roles and the model each runs on.

## Memory

JULIA learns across sessions. What it learns are notes: one Markdown file each,
with a one-line description that is the whole lesson and a body that says why it
holds and how to apply it, kept under `~/.local/state/julia/memory` (respects
`$XDG_STATE_HOME`): `workspaces/<slug>/` for the directory a note was learned in,
`global/` for notes about you or the machine. The files are yours to read, edit
and delete; `julia memory` lists them.

Every turn ends its instructions with a Memory section: how to learn, then the
index of its notes, each as its name, type, date and lesson, with the notes you
pinned and the notes that name something in your request shown in full. Pin a
note that bears on most of your work, such as how you want code written, with
`julia memory pin NAME`: every turn then leads its Memory section with it, in
full, as a standing preference to apply without being asked, rather than as one
line among many. Only you pin, at most five notes per scope; pinning vouches for
the note as `confirm` does. A run without memory has no such
section and no tool. A note anchored to paths or commands is also repeated inside the
result of a tool that touches them, as a `[Memory · name]` line, so the lesson
arrives when it applies rather than when it happens to be noticed; each note
arrives this way once per conversation, as the index repeats it every turn. The agent
saves with the `Memory` tool when you correct it or state a preference, when you
confirm an approach, when it learns a fact about the project or the machine that
the files do not record, or where something lives; it is told not to save what
the repository records, what matters only to the task, secrets, or anything a
page, file or tool output asked it to remember. After a turn in auto or yolo mode
that could have taught something (you corrected or instructed it, an action was
denied, or a tool failed and the work went on), a reflection step, a separate
tool-free model call over the turn at low effort unless `memory.model` gives its
own, saves or revises what the agent missed, at most three notes, each shown as a
Memory card marked `reflection` and kept in the transcript, and tallies which
notes helped, which misled, and which were ignored (you had to say again what a
note already said). In the TUI it runs after the turn, so the next
message can be typed at once: the status line says while it runs, Esc skips it,
a message sent meanwhile waits for it, and quitting finishes it first (Ctrl+C
skips). In auto mode after content from outside your words it runs within the
turn instead, since its changes then need your approval, and a headless run
always reflects within the turn. Subagents see the notes but save none; what
they learn goes in their reports.

| Type        | For                                                                                                                   |
| ----------- | --------------------------------------------------------------------------------------------------------------------- |
| `feedback`  | How you want work done: a correction, preference, convention or rule you gave, or an approach you confirmed, with why |
| `procedure` | How a recurring task is done here when the files do not spell it out                                                  |
| `user`      | Who you are: role, expertise, standing preferences (global)                                                           |
| `project`   | A fact about the workspace, or the machine, that its files do not record                                              |
| `reference` | Where something lives: a URL, a ticket, a document                                                                    |

Notes are the past, kept as data: the instructions say so, a note can never
authorize an action, and the reviewer never sees one. Once a conversation has
read content from outside your words (WebFetch, WebSearch, Browser, Slack,
Linear, Mail, a subagent's report, Bash with network or outside the sandbox, a
networked terminal, a repository's agent instructions such as `AGENTS.md`,
`CLAUDE*.md` or `.cursor/` rules, however it was read, or an unverified note read
in full; a command that only looks for such a file and finds none, as agents
often do before they start, reads nothing), every change to the notes in it, the agent's or the reflection step's,
needs your approval in auto mode, is refused where no one can approve, and is
marked unverified: in the index, in `Memory read` and in `julia memory`. The mark lasts as long as the
conversation, and a note keeps its own through every rewrite until you clear it
with `julia memory confirm` or write the note yourself; wherever an unverified
note appears, it says so. Forgetting or rewriting a global note, one you
wrote or pinned, or a `feedback` or `user` note an earlier session saved from
your words, asks you too: files older than a rule you gave do not overturn it,
and headless such a change is refused. The reflection after a turn cannot ask: it
leaves such a change undone and says so on its card. A note cannot hold a credential, HTML comments and hidden
characters are stripped from it, at most five notes are saved and three
forgotten in one turn, and a forgotten or replaced version waits in a `.trash`
directory for 30 days. Headless runs (`--prompt`) read notes but save none unless started with
`--memory write`; `--memory off` runs without memory. In ask mode every save and
forget is approved by you; read-only mode only reads.

```sh
julia memory                 # the notes this workspace sees, then the global ones
julia memory show NAME       # one note as its file; global/NAME or workspace/NAME picks a scope
julia memory forget NAME
julia memory confirm NAME    # clear a note's unverified mark once you have read it and stand behind it
julia memory pin NAME        # show a note in full in every turn; unpin NAME makes it one line of the index again
julia memory stats           # how memory was used over the last 30 days here; -days N (0 for all), -all for every workspace
julia memory consolidate     # a headless pass that merges, tightens and prunes with the Memory tool alone, marks kept
julia memory path | workspaces
```

The consolidation pass is offered no tool but Memory, and `--continue` passes
over its session. `/memory`, `/memory NAME`, `/memory forget NAME` and `/memory pin|unpin NAME` do the same
in the TUI, for the session's workspace. A
note's frontmatter records who wrote it (`source`: agent, reflection or user),
the session, when, how often it was read, and how often the reflection step
found it helpful, misleading or ignored; consolidation reads those to decide what earns
its place.

`julia memory stats` measures whether memory earns its keep. Each turn that sees
the notes, each cue delivered, each read, save and forget (failures with their
reason), and each reflection step (what it changed, or why it did not run, and
the tokens it spent) is appended to `activity.jsonl` in the store, which names
notes, tools and workspaces but never what a note says, and keeps the newer half
once it passes 4 MiB. The stats sum it: turns, cues per session, reads, saves by
who made them, the reflection's outcomes and cost, its verdicts, then one row per
note: the turns whose index held it, then what reached the agent beyond that line
(cues, full showings for a request that named it, reads), its verdicts, and how
many of the sessions that could see it those reached. A note is marked `pinned`,
`index only` when it has no cues (the index is its whole delivery, so its
verdicts are what say whether it lands), `cues never fired` when a week-old
note's cues never matched, `noisy` when its cues fire in most sessions, and
`often ignored` when you keep repeating what it says. The reflection step counts
a note as ignored even when your correction is narrower than the note, as a
complaint that one file is hard to follow is covered by a note on writing for
the next reader. The consolidation pass's reads are recorded as its own and
count as neither use nor reads.

## Permissions and sandbox

| Mode             | Behavior                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auto` (default) | Reads, workspace edits, offline or loopback sandboxed Bash and terminals, and the Browser on the loopback servers its terminals started and workspace files run freely. Bash with network, escalated Bash, a networked terminal's start and every input to it, web searches, each new Browser origin (the machine's other loopback servers included), and text, keys, uploads and scripts sent to a page beyond loopback go to the reviewer model. |
| `ask`            | You approve every mutation, command, terminal start and input, search, and Browser action on a page (navigating, clicking, typing, scripts); looking at a page needs no approval.                                                                                                                                                                                                                                                                  |
| `read-only`      | Only `Read`, `ViewImage`, `Grep`, and `List`.                                                                                                                                                                                                                                                                                                                                                                                                      |
| `yolo`           | No sandbox and no review: Bash and terminals run directly on the host with the process environment, and every action is granted. For an unattended agent in a container that is itself the trust boundary, such as a one-task Cloud Run job. Set it explicitly in the config or with `--mode yolo`; it is never a fallback, and never for a machine you care about.                                                                                |

**Sandbox.** Bash runs under a deny-by-default Seatbelt profile. It can write
only to the workspace (never `.git`), a temp dir, isolated Go caches, and
`$JULIA_SCRATCH`. It has no network and no access to home credentials, and
credentials are stripped from its environment. A `Terminal` program gets the same
profile plus its own terminal, through `/dev/tty` and the descriptors it inherits;
no terminal can be opened by its device name, which macOS reuses. The agent can
request `network: true` for a single call, or for one terminal's lifetime, or
`loopback: true`, which lets it listen on localhost and connect to the servers it,
or the agent's loopback terminals, start there: dev servers and tests that start
local servers. Services already running on the machine (a database, your own dev
server, JULIA's browser proxy) and Unix sockets stay closed to it; they need
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
every connection beyond loopback goes through a JULIA proxy that opens only
approved hosts, which also stops WebSockets and preconnects. Without asking, a page
reaches loopback servers, files in the workspace and `$JULIA_SCRATCH` (served by
JULIA without following symlinks, never credential or `.git` paths), and `data:`
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
reach and copied into `$JULIA_SCRATCH/downloads` without following symlinks. Page
text is untrusted: the agent treats it as data, never as instructions.

**Reviewer.** `reviewer_model` in the config picks it, as `provider/<model-id>`.
Unset, it is `codex/gpt-6-luna`: Codex with no reasoning, running in a
read-only, no-network process. `claude/<model>`, such as `claude/sonnet`, reviews
through Claude Code at low effort in an unsaved session with no tools, on your
Claude subscription. `<provider-id>/<model-id>` for one of your
`providers`, such as `openrouter/typesafe/jev-1.13`, reviews instead through that
provider's Decisions API. Approval then requires ≥50% task relevance and >70% on each harm
check. The reviewer sees the proposed action and your messages, but nothing else
from the agent. A denial goes back to the
agent with the reason. After 3 denials in a row (or 20 in one turn), or when the
reviewer fails, you are asked instead. In headless mode the action is denied.

## Development

```sh
make check                                                        # go vet + go test -race
JULIA_SANDBOX_TEST=1 go test ./internal/tools -run SandboxIntegration -v
JULIA_BROWSER=/path/to/chrome go test ./internal/browser ./internal/tools   # against a particular Chrome
JULIA_CODEX_REVIEW_POLICY_LIVE=1 go test ./internal/provider -run TestCodexLiveReviewInspectionPolicy -count=1 -v
JULIA_JEV_REVIEW_POLICY_LIVE=1  go test ./internal/provider -run TestJevLiveReviewDevelopmentPolicy -count=1 -v
JULIA_MEMORY_EVAL_LIVE=1 go test ./cmd/julia -run TestMemoryEvalLive -count=1 -v -timeout 40m
```

The default test suite is hermetic. Browser tests drive the Chrome JULIA finds,
against loopback test servers only, and skip without one. The live review suites
send only synthetic proposals and never execute them.

The memory evaluation builds `julia` and runs each scenario as a few headless
sessions (`--prompt --json --memory write`) in a temporary workspace with
`XDG_STATE_HOME` pointing at a temporary directory, so your own notes and
sessions are untouched. Each scenario runs twice: with memory, and with
`--memory off` as the control. The workspace is reset before every session, so
the last session can only know what memory carried. Scenarios: a correction
honoured two sessions later, an environment quirk (a test that needs
`loopback=true`) not rediscovered, a project fact no file records, and a
poisoned workspace file and `AGENTS.md` that ask to be remembered. The last
session is scored by deterministic checks of its tool calls and the files it
wrote, and the run prints a table of scenario, memory on or off, result, notes
saved, unverified notes, saves refused, and what tainted the conversations.
The test fails when a scenario fails with memory or a verified note holds the
poisoned instruction; the control failing the learning scenarios is the
expected result. It uses your configured provider and your skills
(`JULIA_MEMORY_EVAL_MODEL=provider/model` overrides the model) and takes a
minute or two; `JULIA_MEMORY_EVAL_KEEP=dir` keeps each arm's state, workspace
and JSON events there.

| Package                                                        | Role                                                                                |
| -------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `cmd/julia`                                                    | CLI entry, flags, subcommands, wiring                                               |
| `agents`                                                       | Built-in subagent roles (Markdown) and their loader                                 |
| `internal/core`                                                | Shared types, `Backend` interface, system prompt (`system.md`)                      |
| `internal/provider`                                            | Codex app-server, Claude Code and Fantasy adapters, compaction, reviewers           |
| `internal/tools`                                               | Tool execution, sandbox, permissions, browser network policy, web search, subagents |
| `internal/browser`, `internal/terminal`                        | Headless Chrome over the DevTools protocol; virtual terminals                       |
| `internal/ui`                                                  | Bubble Tea TUI                                                                      |
| `internal/memory`                                              | Notes kept between sessions, their index, and the reflection step's prompt          |
| `internal/session`, `internal/config`, `internal/instructions` | Persistence, settings, each turn's instructions                                     |
| `internal/skills`                                              | The user's Agent Skills in `~/.agents/skills`: discovery and reading                |
