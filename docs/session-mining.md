# Session mining

The miner turns selected saved JULIA conversations into a supervised fine-tuning
dataset. It reads source sessions without changing them. It does not execute old
tool calls, train a model, or modify LM Studio.

## Workflow

Create a small run first:

```sh
RUN="${XDG_STATE_HOME:-$HOME/.local/state}/julia/mining/$(date -u +%Y%m%dT%H%M%SZ)"
julia mine scan --all --limit 100 --output "$RUN"
julia mine review "$RUN"
julia mine judge "$RUN"
julia mine review "$RUN"
```

The first review is optional: it lets you inspect transcripts and reject or defer
private examples **before** sending anything to a model. Unjudged examples cannot
be accepted for training. Rejected and deferred examples are not sent for judging;
clear their decision with **u** if you want to judge them later.

The judge is your configured model. To choose another provider/model, use the
existing global `--model` or `--profile` flag before `mine`. The judge command
prints its selected model. A cloud provider receives the selected transcripts,
following feedback, focus, and tool definitions. A configured local endpoint keeps
those requests on that endpoint. No credentials or models are selected automatically
from the source sessions.

Reports live in a new directory beneath JULIA's state directory by default, not in
your repository. `--output` must name a new directory. Files are owner-only.
A report contains private transcripts; keep it out of Git and shared folders.

### Scan

`scan` defaults to the current workspace. Global `--dir` selects another
workspace; `--all` includes every workspace.

| Flag | Meaning |
| --- | --- |
| `--source-provider` | Exact source provider ID |
| `--source-model` | Exact source model ID |
| `--since YYYY-MM-DD` | Sessions updated on or after this UTC date |
| `--limit 100` | Maximum shortlisted candidates |
| `--max-bytes 49152` | Maximum encoded transcript plus following feedback |
| `--focus TEXT` | Behavior the judge should evaluate |
| `--output DIRECTORY` | New report directory |

Provider/model filters use the session's saved selection. Sessions do not record
the model separately for every message; a session that switched models can contain
mixed provenance.

A candidate is one complete user turn: the request, assistant messages, calls,
results, and final answer. Earlier turns are not silently truncated into an
incomplete example. The judge must flag a candidate that depends on missing context.
Recognized following user feedback is judgment evidence only, never training input.

The local shortlist balances tool use, direct answers, possible permission
questions, corrections, and blockers. These signals rank candidates for inspection;
they are not quality labels. Exact duplicate transcripts are removed after
normalizing call IDs and argument formatting. This is not semantic deduplication.

The scanner skips malformed sessions, unsupported tools or arguments, unresolved
calls, known interruptions, images, oversized turns, and common secret patterns.
A turn you steered with a message mid-turn is skipped whole: split at the steer,
it would give a fragment that stops mid-task and a continuation without the
request. The scanner finds steers in the session's audit. Sessions saved before
JULIA audited steers do not record them, so check older turns for one in review.
Root conversation groups containing private messaging or Linear tool calls are
excluded entirely, even when only a subagent used them or source filters exclude
that child. Parent results and later messages can quote private content. Recorded
post-turn memory-reflection bookkeeping is removed from otherwise complete turns.
Skip counts appear in the report.
Secret detection is heuristic: it does not certify a transcript as safe.

### Judge

```sh
julia mine judge --call-timeout 2m "$RUN"
```

Each open, unjudged candidate gets one isolated, tool-free completion. The judge
evaluates initiative, completion, restraint, context completeness, and sensitivity,
and returns a recommendation with evidence. The default focus is bounded initiative,
not tool-call frequency.

Results and token usage are saved after every attempt. Rerunning judges only
remaining open candidates; it does not change votes or successful assessments.
Failed calls remain retryable. Use the same configured model identifier to resume;
a different judge requires a new scan. Malformed model responses are rejected
rather than treated as approvals.

`report.json` is the authoritative checkpoint. `report.md` is a readable summary
refreshed after each command. It contains assessments and source references, not
complete transcripts. One command can modify a report at a time.

### Review cards

```sh
julia mine review "$RUN"
```

| Key | Action |
| --- | --- |
| Left/right or p/n | Previous/next card |
| Up/down, Page Up/Page Down | Scroll the full card |
| 1–5 | Accept and rank: 1 lowest, 5 best |
| r | Reject |
| d | Defer |
| u | Clear the decision |
| e, then y | Confirm export |
| q or Esc | Quit; saved decisions remain |

There is no default acceptance. Each decision is saved before advancing.
A failure before file replacement leaves the prior decision intact. If replacement
succeeds but the durability check fails, the UI keeps the committed vote and shows
a warning. Judge rejection is advisory:
you may accept an example you consider good. Flags for sensitive content, missing
context, or a target needing correction block acceptance and export.

This version does not rewrite failures. Keep them in the report as repair
candidates; do not teach their original assistant responses as good behavior.

### Export

Press **e**, then **y**, in the review, or run `mine export`. Both take the same
export flags:

```sh
julia mine export --min-rank 4 --validation-percent 10 "$RUN"
julia mine review --min-rank 4 --output "$RUN/dataset-rank4" "$RUN"
```

A new `dataset/` directory beside the report contains:

- `training.jsonl`: human-accepted examples meeting the minimum rank.
- `validation.jsonl`: held-out examples, with the same format.
- `manifest.json`: source session IDs, message ranges, parent groups, ranks,
  split assignments, row hashes, judge identifier, and normalization policy.

Export never overwrites an earlier dataset. For another export, provide a new
`--output DIRECTORY`.

The default holdout is 10%, assigned by a stable hash of each root conversation ID.
A parent session and its subagents stay together. Percentages apply to groups,
not rows; a small run can have no validation examples or only validation examples.
An all-validation export is refused. For a training-only experiment, explicitly use
`--validation-percent 0` and arrange a separate evaluation set.

Grouping prevents splitting one conversation across train and validation. It does
not detect related tasks in separate conversations or copies with changed wording.
Check those before using validation results as evidence of improvement.

## Training format

Each line is an object with `messages` and `tools`, using OpenAI-style function
calls. Function `arguments` is a JSON-encoded **string**; `tool_call_id` links each
result to its call. Call IDs and argument formatting are normalized. Report fields,
rank, assessments, and following feedback are not inserted into training messages.

Source sessions do not save their full system instructions or historical tool
schemas. Export therefore supplies a short, explicit bounded-initiative system
policy and the current tool definitions captured at scan time. Calls must validate
against those schemas. This is a normalized demonstration, not an exact replay of
the teacher model's input.

Use a trainer that:

1. Applies the target model's tool-aware chat template, including `tools` and
   tool-call/result turns.
2. Trains on assistant tokens only, not user text or tool results.
3. Rejects or correctly handles examples longer than the training context window,
   rather than dropping the original request.

Some trainers expect function arguments as objects rather than JSON strings.
Convert them during dataset loading if the target template requires it. A trainer
that reads only role/content and ignores `tools` or `tool_calls` will silently lose
the behavior this dataset is meant to teach.

No particular Nemotron/MLX trainer or LM Studio adapter-loading workflow is
validated by this export. The file is a portable tool-calling dataset, not a claim
that every fine-tuning command can consume it unchanged.
