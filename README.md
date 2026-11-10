# JULIA

**J**ust **U**sing **L**anguage **I**ntelligently, **A**pparently

A terminal coding agent written in Go (Bubble Tea / Fantasy). It runs on your
Codex or Claude subscription or any compatible API providers. It executes Bash in a macOS
Seatbelt sandbox, and sends risky actions to a separate reviewer model before
they run.

![JULIA welcome screen](docs/welcome.png)

Note: This harness is designed to be opinionated. Underlying models are highly configurable but tools, workflows and the TUI are fixed. If you do not like it, this harness is not for you and I would highly recommend you give something like [Pi](https://pi.dev) a try.

Also note: This harness is largely slop (just as Codex and Claude is). The codebase is a mess and should not be viewed by the faint of heart. However, it works and is a joy to use. The sandboxing limits and auto-approvals are however very well-tested. Running this harness in auto mode should be considered as safe as using auto mode in a harness like Codex or Claude Code, possibly safer as it is quite conservative comparatively in my experience.

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
julia -p "fix the build" [--json]   # headless, one task; -p is short for --prompt
julia --demo                  # offline UI preview
julia --profile work          # run on ~/.config/julia/config.work.toml (or .json)
julia sessions | login | doctor | config init
julia memory [review | show NAME | forget NAME | confirm NAME | pin NAME | unpin NAME | stats [--days N] [--all] | consolidate | path | workspaces]
```

Other flags: `--model`, `--reasoning`, `--mode`, `--memory off|read|write`,
`--timeout`, `--config`, `--profile`. Run `julia -h`, or `julia COMMAND -h`,
for the full list.

`--resume` and `--continue` reuse the session's saved profile or explicit config
file. Pass `--profile` or `--config` to override it. Sessions saved before this
metadata was added use the launch default instead.

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
| Ctrl+O                        | Expand/collapse tool output, full subagent tasks, and service-tool arguments                    |
| Ctrl+T                        | Show/hide provider thinking, live or saved, in the conversation and fleet                                               |
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
| `/`, then ↑↓, Tab, Enter      | Command menu: every command and skill that matches what you type · Tab completes · Enter runs · Esc closes             |
| `/SKILL`, `/SKILL INPUT`      | Run one of your [skills](#skills), with what follows as its input · `/skill:SKILL` when a command has its name         |

Thinking is hidden by default. Ctrl+T shows only what the provider returns;
Codex supplies reasoning summaries, and some models supply no thinking text.
Saved sessions retain this output separately from replies.

During an approval prompt, `y`/Enter allows the action once (a Browser origin, for
the rest of the turn); any other key denies it.

## Providers

API-provider streaming requests retry HTTP 429 up to five total attempts per
step, with delays of 5, 10, 20 and 40 seconds. Valid `Retry-After` values take
precedence. No retry starts after the two-minute retry window; an in-flight
request keeps its normal timeout. JULIA reports retry progress and does not
replay partial responses or completed tool actions. This policy does not change
non-streaming compaction/review calls. Codex and Claude Code manage their own
provider retries.

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

`julia config init` creates `~/.config/julia/config.toml` (respects
`$XDG_CONFIG_HOME`). Existing JSON configurations remain supported. If both
`config.toml` and `config.json` exist, TOML wins; an invalid TOML file is
reported rather than falling back to JSON. If only JSON exists, it remains the
default, including for `config init`, which refuses to overwrite it. With
neither file present, init creates TOML.

`--config PATH` selects that file without profile resolution: a `.toml`
extension selects TOML; other paths use JSON for compatibility. A shared file
beside it is still loaded as described below. `config init` writes that format
and never overwrites an existing file or copies shared settings into it.
`--config` and `--profile` cannot be combined. Flags override configuration.
Repo-local config is never loaded automatically, and keys are only referenced
by env var name.

Full TOML reference (JSON accepts the same fields):

```toml
model = "codex/gpt-6-astra"
reasoning = "medium"
mode = "auto"
max_steps = 256
context_tokens = 256000
timeout = "4h"
use_usage_resets = false
enabled_models = [
  "codex/gpt-6-astra",
  { id = "openrouter/vendor/model-b", reasoning = "high", context_window = 1000000 },
]
workspace_instructions = ["AGENTS.md"]
disabled_tools = ["WebSearch", "Subagent"]
system_prompt = "compact"
browser = "/Applications/Chromium.app"

[[providers]]
id = "openrouter"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"

[browser_auth."https://grafana.example.com"]
Authorization = "Bearer $(gcloud secrets versions access latest --secret=grafana-token --project=example)"

[subagents]
scout = "openrouter/vendor/fast-model"
reviewer = { id = "openrouter/vendor/model-b", reasoning = "high" }
oracle = "codex/gpt-6-astra"

[memory]
reflection = "on"
model = "codex/gpt-6-astra"
index_bytes = 16384
```

- A nonempty `enabled_models` is the allowlist for model selection: the picker, `/model provider/ID`, `--model`, and Ctrl+N offer only those models. When it is absent or empty, selection is unrestricted and Ctrl+N has no list to cycle. The configured profile model and a resumed session's model can remain active even when not on the list; they are not offered for selection. Switching profiles loads the new profile's model and allowlist.
- `model` and each `enabled_models` entry are `provider/<model-id>`, or a TOML inline table such as `{ id = "provider/<model-id>", reasoning = "high", context_window = 128000 }` (a JSON object in JSON). The model starts at that effort when you launch with it or switch to it, and has a context window of its own. `reasoning` and `context_window` are optional.
- `use_usage_resets` defaults to `false`. When enabled, Codex can redeem an available reset credit after included usage runs out and continue the task, with at most one redemption attempt per provider run. It never buys credits or enables paid extra usage. Claude is not supported because no reset-redemption operation has been verified for its headless protocol.
- `context_tokens` is the assumed window for every model without a `context_window` of its own. API providers compact the history at 85% of the model's window. The sidebar uses the window reported by Codex or Claude Code when available, with these settings as the fallback.
- `workspace_instructions` names files at the workspace root, such as `AGENTS.md` or `CLAUDE.md`, whose contents join every turn as the repository's own context, labelled as such and without authority over the task. Omitted, nothing is read from a repository: a cloned project's files are not trusted by default. A missing file is skipped; a symlink or a file over 64 KiB is a warning.
- `system_prompt` is `full` (the default) or `compact`: the same system prompt with JULIA's working rules in a shorter form (`internal/core/system-compact.md`), for a small local model trained on them. Your `system.md`, the runtime context and the skills stay the same; subagents get the same form.
- `disabled_tools` names tools, such as `WebSearch` or `Subagent`, that neither the agent nor its subagents are offered; a call to one anyway is refused. A benchmark uses it to give every model the same tool set. Omitted, every tool is offered.
- The last reasoning effort you picked for each provider is remembered in `~/.local/state/julia/preferences.json`. It applies at launch unless the model has its own `reasoning`, and `--reasoning` beats both.
- Every model call is timed, and `~/.local/state/julia/throughput.json` keeps each model's last week of calls. The model picker (`ctrl+p`) shows each model's output tokens per second over them, with how long ago it was last used once that is more than a week. A model's old calls are dropped only when it is used again, so an idle model keeps its figures.
- `subagents` assigns a model to each [subagent role](#subagent-roles) by name, as `provider/<model-id>` or `{"id": ..., "reasoning": ...}`. A role without one runs on the delegating agent's model. An assigned model starts at its entry's effort, else at the parent's when it is the parent's model, else at the one `model` or `enabled_models` gives it.
- `memory` keeps notes between sessions (see [Memory](#memory)): `reflection` is `on` (the default) or `off`, `model` runs the reflection step on another model, as `provider/<model-id>` or `{"id": ..., "reasoning": ...}`, and `index_bytes` caps what the index of notes may take of the instructions (4096 to 65536; default 16 KiB). The reflection step runs at low effort unless `model` gives its own.
- `browser` is the Chrome the Browser tool launches (an executable or `.app`); `JULIA_BROWSER` overrides it. Without either, JULIA looks for Chrome, Chromium, Chrome for Testing, Brave or Edge in `/Applications`, then in Playwright's and Puppeteer's caches. `npx playwright install chromium` provides one.
- `browser_auth` signs the Browser tool in to sites that take a token, such as a Grafana service-account token. For each origin (`https://host` or `https://host:port`, as the browser writes it), it gives the headers that origin's requests carry. Each value is expanded by `/bin/bash` the way it would expand a double-quoted string, so `$VAR` and `$(command)` work, and a command that fails stops the expansion. It runs on the host, outside the sandbox, the first time a turn may reach the origin: once it is approved, or when it is a server the agent's terminals started. The agent sees which headers were sent, never their values. They go with the origin's own requests and with top-level GET navigations to it, not with other sites' requests to it, and not across a redirect to another origin.

### Custom system prompt

`system.md` beside the configuration file, usually `~/.config/julia/system.md`, is your
own part of the system prompt: who you are, how JULIA should address you, how it
should answer. Its text goes right after JULIA's opening lines, ahead of the
working rules, for every agent and subagent. It is read again each turn, so an
edit applies to the next message.

### Skills

JULIA offers every [Agent Skill](https://agentskills.io/specification) in
`~/.agents/skills`: each folder there whose `SKILL.md` frontmatter gives a
`name` matching the folder and a `description`. Every turn's instructions list
each skill's name and description, and the model loads a skill's instructions,
or a file beside them, with the read-only Skill tool when a task calls for it.
A symlinked skill folder is followed; nothing outside a skill's folder can be
read through the tool. A malformed skill is left out with a warning. `julia
doctor` lists what it finds. The old `skill_paths` key is ignored.

Every skill is also a slash command: `/code-review since main` sends your
message as typed, with the skill's `SKILL.md` added for the model, which takes
what follows the command as the skill's input; the action reviewer reads it as
part of your request. A skill whose name is a built-in command's runs as
`/skill:<name>` instead. Skills marked `disable-model-invocation: true` are
commands only: the model is never offered them, but one you run can load the
files beside it for that turn. Typing `/` opens a menu of the commands and
skills that match; the skills folder is read again each time it opens.

### Profiles

Each `config.<name>.toml` or `config.<name>.json` in the configuration
directory is a profile named `<name>`; `config.toml` or `config.json` is the
`default` profile. `julia --profile work` selects `config.work.toml` when
it exists, otherwise `config.work.json`. If neither exists,
`julia --profile work config init` creates `config.work.toml`. When both
formats exist for a profile, TOML wins and the profile is listed once; an
invalid TOML file does not fall back to JSON. An existing profile is not
overwritten by init. Profile names are letters, digits, `-` and `_`.

`config.shared.toml` (or `config.shared.json`) beside the active configuration
provides shared settings for every profile and for an explicit `--config` path.
TOML wins if both shared formats exist; an invalid TOML file is reported, not
replaced by JSON. The default profile works with only a shared file, but a named
profile must still exist. `shared` is reserved and is not a selectable profile.
Selecting a shared file explicitly with `--config` loads that file once.

The active file overrides shared settings. Tables and objects merge recursively;
scalars and arrays replace the shared value, including explicit empty and zero
values. Model strings replace model objects and vice versa. Both files must
conform to the configuration schema; semantic validation runs after the merge.
Within each file, duplicate schema fields are rejected, including names that
only differ in case. Dynamic map keys, such as header names and subagent roles,
remain case-sensitive. Defaults apply to settings absent from the result.
`config init` writes its usual defaults, which may override shared values until
you edit the new file.

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
`Linear` (when an access token is saved), `GitHub` (with `gh` or a token),
`Bugsnag` (read-only, with a personal auth token),
`Mail` (when a mailbox's credentials are saved), `Subagent` (up to 3 parallel child tasks), `Memory` (see [Memory](#memory)),
`Skill` (see [Skills](#skills)), and `AskUserQuestion` (interactive only).
`Read`, `List`, `Grep`, `Edit`, `MultiEdit` and `Write` accept relative or absolute paths.
Absolute paths inside the workspace follow the same rules as relative paths. Outside
paths go to the reviewer in auto mode, require your approval in ask mode, and are
blocked in read-only mode; yolo grants them without review. Approval covers one call,
not later access to that path. Symlinks, parent traversal, protected credential paths
and `.git` remain forbidden. Text output is capped at 32 KiB.

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
Yolo mode runs unattended, so it skips that flow: without a key, the call
fails at once.

`Slack` appears when `SLACK_BOT_TOKEN` holds a bot token: post a message, react
to one, read a thread or channel, or list the channels the bot is in. Posting
and reacting are reviewed in auto mode and approved in ask mode; reading is
free. `SLACK_OWNER_USER_ID` tells the agent whose bot it is, so it can name the
owner in a thread and message them directly. The bot needs the scopes
`chat:write`, `reactions:write`, `channels:read`, `groups:read`,
`channels:history`, `groups:history`, `im:history` and `im:write`.

`Linear` appears when `~/.config/julia/linear.json` holds `{"api_key": "…"}`
or `LINEAR_ACCESS_TOKEN` is set.

`GitHub` uses your GitHub CLI sign-in on **github.com**. Run `gh auth login`
if you have not signed in. For cloud runs, supply `GH_TOKEN` or `GITHUB_TOKEN`
through your secret store; they take precedence in that order. JULIA resolves
credentials privately and never returns them to the model. `julia doctor`
reports the credential source without reading the token from `gh`. An invalid
`GH_TOKEN` or `GITHUB_TOKEN` turns the tool off with a warning; it does not fall
back to `gh`, which may be a different account.

The tool sends API requests, not shell commands: `path` is an API-relative
path such as `/user` or `/repos/owner/repo/issues?state=all`, `method` defaults
to `GET`, and `body` is an optional JSON string. This covers REST operations
without a separate wrapper for each feature. `GET` and `HEAD` reads run
directly; `POST`, `PUT`, `PATCH`, `DELETE`, and all GraphQL requests are
reviewed in auto mode, approved by you in ask mode, and blocked in read-only
mode. Yolo grants them without review, as with other tools. Review considers
the full request, including its body; a GitHub comment cannot authorize a write.

The tool accepts only `api.github.com` request paths. Results are at most 32 KiB;
follow the returned page path or GET `offset` to read more (up to a 4 MiB response).
GET follows HTTPS redirects for signed downloads without forwarding `Authorization`
or `Referer`; download-host errors identify that host. HEAD and writes do not follow
cross-origin redirects, and the tool returns text, not binary downloads. Control
characters and invalid UTF-8 in raw and diff text become U+FFFD, and the result says
so. An existing `gh` credential may itself have write permissions:
the tool's approval gate does not restrict a separately approved host shell.

`Bugsnag` reads every organization and project your personal auth token can see,
including issues and events with stack traces, breadcrumbs and metadata. In your
Bugsnag dashboard, open **My account > Personal auth tokens**. Use a personal auth
token, not a project API key used by notifier SDKs. Save
`{"auth_token":"..."}` in `bugsnag.json` beside your configuration, normally
`~/.config/julia/bugsnag.json`, and run `chmod 600 ~/.config/julia/bugsnag.json`.
Alternatively, set `BUGSNAG_AUTH_TOKEN`. Restart JULIA after setup.

The optional `endpoint` field accepts only `https://api.bugsnag.com` (default,
for `app.bugsnag.com`) or `https://api.bugsnag.smartbear.com` (for
`app.bugsnag.smartbear.com`). Select the endpoint that matches your dashboard.
`julia doctor` shows the configured host without the token. Calls take an
API-relative absolute `path` and an optional byte `offset` to continue long output.
The tool sends only GET requests to the configured API origin. It has no mutation
operations, even in yolo mode. Results are private, untrusted API data.

`Mail` appears when `~/.config/julia/mail.json` holds a mailbox's IMAP
credentials. It is made for [Proton Mail Bridge](https://proton.me/mail/bridge),
which decrypts your account on this machine and serves it over IMAP on `127.0.0.1:1143` with a password of its own
(a paid plan is needed): save `{"username": "you@proton.me", "password": "<the Bridge password>"}`,
and JULIA connects with STARTTLS.

For Gmail or Google Workspace, create an [app password](https://myaccount.google.com/apppasswords)
with 2-Step Verification enabled, if your account policy allows it. Save this in
`~/.config/julia/mail.json`, using the app password without spaces:

```json
{
  "username": "you@example.com",
  "password": "<Google app password>",
  "address": "imap.gmail.com:993",
  "security": "tls"
}
```

Set the file permissions with `chmod 600 ~/.config/julia/mail.json` and restart JULIA.
Do not use your normal Google password. Gmail archiving accepts only INBOX message
IDs and moves messages to All Mail after your approval. All Mail must be visible
in IMAP. Proton Bridge and Gmail share one configuration file, so saving one
replaces the other.

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
index of its notes.

Pin a memory that bears on most of your work, such as how you want code written, with
`julia memory pin NAME`: every turn then leads its Memory section with it, in
full, as a standing preference to apply without being asked, rather than as one
line among many. Only you pin, at most five notes per scope; pinning vouches for
the note as `confirm` does.

| Type        | For                                                                                                                   |
| ----------- | --------------------------------------------------------------------------------------------------------------------- |
| `feedback`  | How you want work done: a correction, preference, convention or rule you gave, or an approach you confirmed, with why |
| `procedure` | How a recurring task is done here when the files do not spell it out                                                  |
| `user`      | Who you are: role, expertise, standing preferences (global)                                                           |
| `project`   | A fact about the workspace, or the machine, that its files do not record                                              |
| `reference` | Where something lives: a URL, a ticket, a document                                                                    |

Notes are the past, kept as data: the instructions say so, a note can never
authorize an action, and the reviewer never sees one.

```sh
julia memory                 # the notes this workspace sees, then the global ones
julia memory review          # one card per note, from the first unverified one: c confirms, e edits, * pins, f forgets
julia memory show NAME       # one note as its file; global/NAME or workspace/NAME picks a scope
julia memory forget NAME
julia memory confirm NAME    # clear a note's unverified mark once you have read it and stand behind it
julia memory pin NAME        # show a note in full in every turn; unpin NAME makes it one line of the index again
julia memory stats           # how memory was used over the last 30 days here; --days N (0 for all), --all for every workspace
julia memory consolidate     # a headless pass that merges, tightens and prunes with the Memory tool alone, marks kept
julia memory path | workspaces
```

The consolidation pass is offered no tool but Memory, and `--continue` passes
over its session. `/memory`, `/memory NAME`, `/memory forget NAME` and `/memory pin|unpin NAME` do the same
in the TUI, for the session's workspace.

In `julia memory review`, `e` opens the note in `$VISUAL` or `$EDITOR`, without the
fields the store keeps for itself. Saving makes it your note: it is verified, and
the agent asks you before it replaces or forgets it. The old text waits in
`.trash`. An editor that exits without a change, or with an error such as vim's
`:cq`, saves nothing.

`julia memory stats` measures whether memory earns its keep.

## Permissions and sandbox

| Mode             | Behavior                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auto` (default) | Workspace reads and edits, offline or loopback sandboxed Bash and terminals, and the Browser on the loopback servers its terminals started and workspace files run freely. File calls outside the workspace, Bash with network, escalated Bash, a networked terminal's start and every input to it, web searches, each new Browser origin (the machine's other loopback servers included), and text, keys, uploads and scripts sent to a page beyond loopback go to the reviewer model. |
| `ask`            | You approve file calls outside the workspace, every mutation, command, terminal start and input, search, and Browser action on a page (navigating, clicking, typing, scripts); looking at a page needs no approval.                                                                                                                                                                                                                                                                  |
| `read-only`      | Workspace reads (plus scratch images) and read operations in configured service tools; no mutations, shell commands, Browser actions, or web search/fetch.                                                                                                                                                                                                                                                                                                                                                                                                      |
| `yolo`           | No sandbox and no review: Bash and terminals run directly on the host with the process environment, and every action is granted. Set it explicitly in the config or with `--mode yolo`. It is never a fallback, and never for a machine you care about.                                                                                                                                                                                            |

## Sandbox

Bash runs under a deny-by-default Seatbelt profile. It can write
only to the workspace (never `.git`), a temp dir, isolated Go caches, and
`$JULIA_SCRATCH`. It has no network and no access to home credentials, and
credentials are stripped from its environment.

A `Terminal` program gets the same profile plus its own terminal, through `/dev/tty` and the descriptors it inherits.
No terminal can be opened by its device name, which macOS reuses. The agent can request `network: true` for a single call,
or for one terminal's lifetime, or `loopback: true`, which lets it listen on localhost and connect to the servers it,
or its other loopback terminals started.

Services already running on the machine (a database, your own dev server, JULIA's browser proxy)
and Unix sockets stay closed to it; they need `network: true`.
Auto mode runs loopback commands without review, since they reach only what the agent itself started.

Seatbelt cannot tell a wildcard listener from a loopback one, so a server bound to
every interface still accepts the local network, as it would if you ran it; bind
127.0.0.1 to avoid that.

A Bash call can also request `sandbox_permissions: "require_escalated"` with a justification, which
runs that exact command on the host after approval. Terminals never leave the sandbox.
If the sandbox can't be set up, the command fails rather than running unsandboxed.

## Reviewer

`reviewer_model` in the config picks it, as `provider/<model-id>`.

Unset, it is `codex/gpt-6-luna` - Codex with no reasoning, running in a read-only, no-network process.

`claude/<model>`, such as `claude/sonnet`, reviews through Claude Code at low effort
in an unsaved session with no tools, on your Claude subscription.

`<provider-id>/<model-id>` for one of your `providers`, such as `openrouter/typesafe/jev-1.13`,
reviews instead through that provider's Decisions API. Approval then requires ≥50% task relevance and >70% on each harm
check. The reviewer sees the proposed action and your messages, but nothing else from the agent.
A denial goes back to the agent with the reason. If the agent repeats the same action and you have not answered
or written since, you are asked instead of the reviewer. You are also asked after 3 denials in a row (or 20 in one
turn), or when the reviewer fails. An escalated command that names a private key or login file (`.ssh`, `.gnupg`,
`.netrc`, `.aws/credentials`, `id_rsa` and similar) always asks you, whatever the reviewer would say. This check
reads the command text, so a glob or a variable gets past it and only the reviewer judges those. In headless mode
the action is denied.

If you have access to Jev or another decision model via API, that is highly recommended.

## Session mining

Mine saved sessions into a reviewed fine-tuning dataset:

```sh
RUN="${XDG_STATE_HOME:-$HOME/.local/state}/julia/mining/$(date -u +%Y%m%dT%H%M%SZ)"
julia mine scan --all --limit 100 --output "$RUN"
julia mine judge "$RUN"
julia mine review "$RUN"
```

Scan is local and does not change sessions. Judge sends the shortlisted transcripts
to your configured model, in isolated calls with no tools. Put `--model` or
`--profile` **before** `mine` to select another judge. Inspect `report.json`, or
use `mine review` before judging to reject sensitive examples locally.

Each candidate is one turn, with the earlier turns of its session as context.
The review cards show the assessment, transcript, tool calls and results, then the
earlier conversation. Press **1–5** to accept and rank, **r** to reject, **d** to
defer, and **e** to confirm export. Decisions persist immediately; both judging and
review can resume.

Export creates `dataset/training.jsonl`, `validation.jsonl`, and a provenance
manifest. Only human-accepted examples are eligible, and only their reviewed turn
has training weight. Missing context, sensitive content and examples needing
correction block export, so the review hides those cards; press **x** to see them.
The first version exports original demonstrations, not model-generated repairs.

See [Session mining](docs/session-mining.md) for filters, privacy, split behavior,
and the tool-aware training format. This prepares data; it does not train or
install a model.

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
| `internal/miner`                                               | Read-only session selection, model judgments, review reports and training export    |
| `internal/session`, `internal/config`, `internal/instructions` | Persistence, settings, each turn's instructions                                     |
| `internal/skills`                                              | The user's Agent Skills in `~/.agents/skills`: discovery and reading                |
