# JULIA

A terminal coding agent written in Go (Bubble Tea / Fantasy). It runs on your
Codex subscription or any of several API providers, executes Bash in a macOS
Seatbelt sandbox, and sends risky actions to a separate reviewer model before
they run.

![JULIA welcome screen](docs/welcome.png)

## Principle: it should be gorgeous

A core principle of this harness is that it should be gorgeous. You spend hours
reading what it shows you, so how it looks is part of how well it works. Every
surface is designed: a code block gets a card, a selection is lit, and a status
line says only what matters. A change that works but looks careless is not done.

## Install

Requires Go 1.27+, `rg`, and macOS (sandboxed Bash is macOS-only for now). For
the default provider, install and sign in to the Codex CLI. JULIA also finds the
copy bundled with the ChatGPT macOS app.

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

| Key | Action |
| --- | --- |
| Enter / Alt+Enter | Send / new line |
| Enter while JULIA works | Queue the message; it is sent when the turn settles (a turn that fails or is interrupted puts it back in the composer) |
| Ctrl+S or the steer button | Steer: hand the queued message, or the draft, to the turn under way, which reads it after its current step |
| ↑ in an empty composer | Take the last queued message back to edit |
| Esc | Cancel the running task |
| Ctrl+P or `/model` | Model picker: every model in one list |
| Ctrl+N | Switch to the next of your `enabled_models` |
| Shift+Tab or `/reasoning` | Change reasoning effort |
| `/profile` or `/profile NAME` | Switch configuration profile |
| Ctrl+O | Expand/collapse tool output, full subagent tasks, and the arguments of Slack and Linear calls |
| Ctrl+F | The fleet: every subagent of the session, with its task, tool calls and report |
| Ctrl+V | Attach the clipboard's image (a screenshot, or an image file copied in Finder) |
| Drop image files | Attach them instead of pasting their paths |
| Drag | Copy the selection (without borders or padding) |
| Click a code block | Copy its code |
| Double-click | Copy a whole message, a tool's command, or its output |
| `/copy`, `/copy code` | Copy the last reply, or its last code block |
| `/review`, `/review WHAT` | Review your uncommitted changes, or what you name (a commit, a path, a PR), and triage the findings |
| `/memory` | What JULIA remembers here · `/memory NAME` shows a note · `/memory forget NAME` deletes it |
| `/clear`, `/help`, `/quit` | New conversation, shortcuts, exit |

During an approval prompt, `y`/Enter allows the action once (a Browser origin, for
the rest of the turn); any other key denies it.

When you ask for a code review, in your own words or with `/review`, JULIA puts its findings in a review card and waits
for your call on each one: **Fix**, **Ignore**, **I fixed it**, or **Reply** with your
own words. ↑↓ moves between findings, ←→ and Enter choose (or `f` `i` `d` `r`), `F`
marks every open finding Fix, Backspace undoes a decision, and Ctrl+S or the Send row
hands the decisions back; JULIA then fixes what you marked. Esc skips the review
(twice once you have decided something). Try it offline with `julia --demo` and `/review`.

Each attached image puts an `[Image #N]` token in your message; delete the token
to drop the image. Images are scaled to at most 2000 px on the long side and sent
to every provider, Codex included. Ctrl+V falls back to an ordinary text paste
when the clipboard holds no image. On Linux it needs `wl-paste` or `xclip`. In Ghostty, sent
images are drawn inline as thumbnails; other terminals show a caption instead.

When JULIA settles while its terminal is in the background (a turn ends with
nothing queued after it, or an approval, question or review card is waiting on
you), it sends a desktop notification and rings the bell. Ghostty, iTerm2,
WezTerm and kitty show the notification; other terminals, Terminal.app among
them, only hear the bell. A focused terminal is left alone. In tmux, set
`allow-passthrough on` and `focus-events on`.

JULIA handles selection itself, because mouse reporting is on and rows carry
borders and the sidebar. Copies go to the system clipboard, or through the
terminal (OSC 52) over SSH.

## Providers

| Provider | Backend | API key env |
| --- | --- | --- |
| `codex` (default) | Local Codex app-server, your ChatGPT sign-in | — |
| `openai` | Responses API | `OPENAI_API_KEY` |
| `anthropic` | Messages API | `ANTHROPIC_API_KEY` |
| `google` | Gemini API | `GEMINI_API_KEY` |
| your `providers` | OpenAI-compatible chat completions (OpenRouter, LM Studio, Ollama, …) | its `api_key_env`, or none |

Models are named `provider/<model-id>`, in the config and in `--model`. A bare
`codex` is the model your Codex is configured with; every other provider needs a
model ID. Reasoning effort is supported on Codex and your `providers`, which are
sent `reasoning_effort` only when you pick one; an endpoint whose model has no
reasoning may ignore it or refuse the request. Endpoints
must be HTTPS unless they are on loopback.

OpenAI-compatible endpoints are named in the config's `providers`, and each `id`
is then a provider of its own:

```json
{
  "providers": [
    {"id": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"},
    {"id": "local", "base_url": "http://localhost:1234/v1"}
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

```json
{
  "model": "codex/gpt-6-astra",
  "reasoning": "medium",
  "mode": "auto",
  "max_steps": 256,
  "context_tokens": 256000,
  "timeout": "4h",
  "providers": [
    {"id": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"}
  ],
  "enabled_models": ["codex/gpt-6-astra", {"id": "openrouter/vendor/model-b", "reasoning": "high", "context_window": 1000000}],
  "skill_paths": ["~/.codex/skills/coding-assistance-style/SKILL.md"],
  "workspace_instructions": ["AGENTS.md"],
  "browser": "/Applications/Chromium.app",
  "subagents": {
    "scout": "openrouter/vendor/fast-model",
    "reviewer": {"id": "openrouter/vendor/model-b", "reasoning": "high"},
    "oracle": "codex/gpt-6-astra"
  },
  "memory": {"reflection": "on", "model": "codex/gpt-6-astra", "index_bytes": 16384}
}
```

- `enabled_models` come first in the model picker, ahead of the rest of Codex's catalog, and Ctrl+N cycles through them.
- `model` and each `enabled_models` entry are `provider/<model-id>`, or `{"id": "provider/<model-id>", "reasoning": "<effort>", "context_window": <tokens>}` for a model that starts at that effort when you launch with it or switch to it, and has a context window of its own. Both keys are optional.
- `context_tokens` is the assumed window for every model without a `context_window` of its own. API providers compact the history at 85% of the model's window, and the sidebar's context gauge measures against it.
- `skill_paths` are extra instruction files added to every turn. If you omit it, JULIA loads `coding-assistance-style/SKILL.md` from `$CODEX_HOME/skills` or `~/.agents/skills`. `[]` disables skills.
- `workspace_instructions` names files at the workspace root, such as `AGENTS.md` or `CLAUDE.md`, whose contents join every turn as the repository's own context, labelled as such and without authority over the task. Omitted, nothing is read from a repository: a cloned project's files are not trusted by default. A missing file is skipped; a symlink or a file over 64 KiB is a warning.
- The last reasoning effort you picked for each provider is remembered in `~/.local/state/julia/preferences.json`. It applies at launch unless the model has its own `reasoning`, and `--reasoning` beats both.
- Every model call is timed, and `~/.local/state/julia/throughput.json` keeps each model's last week of calls. The model picker (`ctrl+p`) shows each model's output tokens per second over them, with how long ago it was last used once that is more than a week. A model's old calls are dropped only when it is used again, so an idle model keeps its figures.
- `subagents` assigns a model to each [subagent role](#subagent-roles) by name, as `provider/<model-id>` or `{"id": ..., "reasoning": ...}`. A role without one runs on the delegating agent's model. An assigned model starts at its entry's effort, else at the parent's when it is the parent's model, else at the one `model` or `enabled_models` gives it.
- `memory` keeps notes between sessions (see [Memory](#memory)): `reflection` is `on` (the default) or `off`, `model` runs the reflection step on another model, as `provider/<model-id>` or `{"id": ..., "reasoning": ...}`, and `index_bytes` caps what the index of notes may take of the instructions (4096 to 65536; default 16 KiB). The reflection step runs at low effort unless `model` gives its own.
- `browser` is the Chrome the Browser tool launches (an executable or `.app`); `JULIA_BROWSER` overrides it. Without either, JULIA looks for Chrome, Chromium, Chrome for Testing, Brave or Edge in `/Applications`, then in Playwright's and Puppeteer's caches. `npx playwright install chromium` provides one.

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
`Linear` (when an access token is saved),
`Subagent` (up to 3 parallel child tasks), `Memory` (see [Memory](#memory)), and
`AskUserQuestion` (interactive only). File tools are confined to the workspace,
refuse symlinks and credential paths, and cap output at 32 KiB.

`ViewImage` shows the model an image from the workspace or `$JULIA_SCRATCH`,
such as a screenshot it rendered, scaled to at most 2000 px. `Terminal` runs a
program on a virtual terminal under the Bash sandbox, so the agent can drive a TUI,
REPL or dev server with keys, text and mouse, and read the screen as text or, when
the look matters, as a rendered screenshot. Up to 4 run at once. At the end of the
turn each is hung up, as closing a terminal window does, and everything still on it
is killed. Ctrl+O shows a terminal's screen in its card, and screenshots appear as
thumbnails in Ghostty.

`Browser` opens pages in JULIA's own headless Chrome, so the agent can check a
web UI as a person would: start the dev server in a loopback terminal, then
navigate, click, type, press keys, select, upload, scroll, drag, wait, evaluate
JavaScript, read the console and resize or switch to dark mode. Every action
answers, once the page has settled (the requests it started get up to 1.5 s), with
the page's accessibility snapshot, or only the lines that changed, plus notes on
popups, dialogs, blocked requests and console errors; a long page is shown around
what is in view, and a screenshot shows the look. A click is refused, as a person's
would miss, when a backdrop or banner covers its target or a container cuts it off.
Chrome starts on the first navigate with a throwaway profile, talks to JULIA over
a pipe rather than a debugging port, gives each subagent its own context, and is
killed with every process it started, profile and crash dumps and all, when the
turn ends, or by a watchdog should JULIA die first. Ctrl+O shows a page's snapshot
in its card, its changes lit as a diff.

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
or `LINEAR_ACCESS_TOKEN` is set: list teams with their workflow states and
labels, list projects and issues, read an issue with its comments, create or
update an issue (labels included: an update adds to the issue's labels unless
`replace_labels` is set), comment on one, or create a label for a team or the
whole workspace with `create_label`. Teams, states, projects, labels and people
are named, never given as ids. Creating, updating, labelling and commenting are
reviewed in auto mode and approved in ask mode; reading is free. The token is an OAuth application's
access token, sent as a Bearer token: in Linear, open Settings → API → OAuth
applications, create an application (a callback URL is required but never
used), and under *Developer token* create one with the actor *Application*, so
everything JULIA does shows as the app rather than as you. Developer tokens
have no refresh token and may expire; when Linear answers 401, create a new one
and replace it in the file. `julia doctor` shows where the token comes from.

### Subagent roles

Each task given to `Subagent` names a role, which adds instructions for that kind
of work, can limit the child to some tools, and can run it on its own model (see
`subagents` above). The built-in roles are Markdown files in [`agents/`](agents):

| Role | For | Tools |
| --- | --- | --- |
| `scout` | Fast reconnaissance: compressed context, with paths and line ranges, for another agent | Read, List, Grep, ViewImage, Bash |
| `researcher` | Web research: a sourced brief that keeps evidence apart from inference | WebSearch, WebFetch, Browser, Read, List, Grep |
| `worker` | Implementation of a well-defined task or approved plan, validated | all |
| `reviewer` | Evidence-backed review of a diff, plan or area of the code, by severity | Read, List, Grep, ViewImage, Bash |
| `oracle` | A second opinion on a decision: drift, contradictions, hidden assumptions | Read, List, Grep, ViewImage, Bash |
| `delegate` | Anything that fits no other role | all |

Ctrl+F opens the fleet, a window over the conversation that lists every
subagent the session has run down its left side; the arrow keys move through
them, and the chosen one's transcript fills the rest: the task it was given, each
tool call as the conversation shows one, with Ctrl+O expanding its output, and
its report. A running agent's pane follows its newest activity; PgUp and PgDn
scroll it. A resumed session's fleet is rebuilt from its children's saved
transcripts.

A role's tools are the only ones its children are offered or can run. Bash can
still write the workspace, so the read-only roles keep to inspection by
instruction. To change a role or add one, put a file of the same shape in an
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

| Type | For |
| --- | --- |
| `feedback` | How you want work done: a correction, preference, convention or rule you gave, or an approach you confirmed, with why |
| `procedure` | How a recurring task is done here when the files do not spell it out |
| `user` | Who you are: role, expertise, standing preferences (global) |
| `project` | A fact about the workspace, or the machine, that its files do not record |
| `reference` | Where something lives: a URL, a ticket, a document |

Notes are the past, kept as data: the instructions say so, a note can never
authorize an action, and the reviewer never sees one. Once a conversation has
read content from outside your words (WebFetch, WebSearch, Browser, Slack,
Linear, a subagent's report, Bash with network or outside the sandbox, a
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

| Mode | Behavior |
| --- | --- |
| `auto` (default) | Reads, workspace edits, offline or loopback sandboxed Bash and terminals, and the Browser on the loopback servers its terminals started and workspace files run freely. Bash with network, escalated Bash, a networked terminal's start and every input to it, web searches, each new Browser origin (the machine's other loopback servers included), and text, keys, uploads and scripts sent to a page beyond loopback go to the reviewer model. |
| `ask` | You approve every mutation, command, terminal start and input, search, and Browser action on a page (navigating, clicking, typing, scripts); looking at a page needs no approval. |
| `read-only` | Only `Read`, `ViewImage`, `Grep`, and `List`. |
| `yolo` | No sandbox and no review: Bash and terminals run directly on the host with the process environment, and every action is granted. For an unattended agent in a container that is itself the trust boundary, such as a one-task Cloud Run job. Set it explicitly in the config or with `--mode yolo`; it is never a fallback, and never for a machine you care about. |

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
read-only, no-network process. `<provider-id>/<model-id>` for one of your
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
expected result. It uses your configured provider and personal skill
(`JULIA_MEMORY_EVAL_MODEL=provider/model` overrides the model) and takes a
minute or two; `JULIA_MEMORY_EVAL_KEEP=dir` keeps each arm's state, workspace
and JSON events there.

| Package | Role |
| --- | --- |
| `cmd/julia` | CLI entry, flags, subcommands, wiring |
| `agents` | Built-in subagent roles (Markdown) and their loader |
| `internal/core` | Shared types, `Backend` interface, system prompt (`system.md`) |
| `internal/provider` | Codex app-server and Fantasy adapters, compaction, reviewers |
| `internal/tools` | Tool execution, sandbox, permissions, browser network policy, web search, subagents |
| `internal/browser`, `internal/terminal` | Headless Chrome over the DevTools protocol; virtual terminals |
| `internal/ui` | Bubble Tea TUI |
| `internal/memory` | Notes kept between sessions, their index, and the reflection step's prompt |
| `internal/session`, `internal/config`, `internal/instructions` | Persistence, settings, skill loading |
