package memory

// ConsolidationPrompt is the task `julia memory consolidate` gives the agent:
// a pass over every note a workspace sees, made with the Memory tool alone,
// that merges, tightens and prunes. Notes accrue one turn at a time, each
// saved without the others in view; consolidation is where they are read
// together, and where what the evidence calls context collapse is avoided by
// changing notes one at a time rather than rewriting the store.
const ConsolidationPrompt = `Consolidate JULIA's memory for this workspace. Use the Memory tool only: list, read, save and forget. Do not read or change workspace files, run commands or start anything; the notes are the whole task. The approval mode in the runtime context binds the workspace tools only: in this run the Memory tool saves and forgets without asking, and a note marked unverified keeps that mark through any rewrite.

1. List the notes, then read every one in full. Each shows when it was last updated, who wrote it, how often it was read, and how often the reflection step found it helpful or misleading.
2. Merge notes that say the same thing into one, keeping the richer body and the clearest description. Saving under an existing name keeps that note's history; when the merged note gets a new name, forget the ones it replaces, and forget them first when the scope is at its limit of notes. A save replaces a note's cues: carry its paths, commands and keywords over unless you mean to change them.
3. Resolve contradictions: the newer note wins unless the older one carries evidence the newer lacks. Say which you kept and why.
4. Tighten every description into one specific line, in the imperative when it is a rule, as later turns will see it. Keep **Why:** (the incident, dated, with the user's words or the evidence) and **How to apply:** in the body. Convert relative dates to absolute ones.
5. Forget what no longer earns its place: notes about finished work, superseded decisions or disproved facts; notes that only restate what the repository documents; notes never read that have misled. Keep what is hard to re-derive: stated preferences and corrections, decisions and their reasons, how this project and this machine differ from the obvious.
6. Anchor notes to their cues where they have clear ones: the paths they apply to, the commands they apply to, the keywords of the requests they apply to. Leave a note that always applies without cues.
7. Move a note to the right scope: global when it is about the user or the machine, workspace when it is about this project.

Finish with a short report: what was merged, rewritten, moved, forgotten and kept, each with its reason in a few words.`
