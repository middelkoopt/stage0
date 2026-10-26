# AGENTS.md — project router & working rules

Always-loaded context for any AI session in this repo: what to read, how to
behave, and the facts that cause damage if wrong. Every line here is paid for
on every session, so keep it short. Project knowledge lives in the files below,
not here. Tool-specific files (such as `CLAUDE.md`) only point at this one and
hold nothing of their own.

**This project:** Stage0 installs one head node (a base OS on disk, reachable
over SSH) from nothing but this repo and a connection to the provisioning
network, so stage-1 provisioners can be CI-tested. Raspberry Pi 5 first.

**Premise:** files persist, conversations don't. The repo is the memory. Every
session starts cold and rebuilds context by reading, so nothing important lives
only in a chat or a tool's memory.

## Session start

1. **Read, in order:**
   1. `handoff.md`, for where work stands now (if there is none yet,
      read `README.md` instead);
   2. whatever the handoff says to read first;
   3. the item the request names, if any (a file, issue, PR);
   4. `docs/index.md` if you need background, then only the docs the task
      needs.
2. **State what you read, what you skipped and why, and what you take the task
   to be.**
3. **Pause before acting, not before learning.** Ask only what the files can't
   answer, then proceed at the right gate:
   - **Read-only work:** go straight ahead.
   - **Writes:** once the human has asked for them or approved your plan.
   - **Anything that reaches another person** (message, comment, PR, post):
     only after the human approves the exact text.

## Where things live (by time-horizon)

Every durable note belongs to exactly one horizon, and the horizon decides its
file. Mixing horizons is the main source of rot. **Start with this file and a
handoff. Create each other file the first time something belongs there, not
before;** if a file named here doesn't exist yet, skip it.

| File | Horizon | Holds | Create it when |
| --- | --- | --- | --- |
| `handoff.md` | present | Status, open decisions, next steps | Day one |
| `docs/` + `docs/index.md` | timeless | How it works as built (design, procedures, gotchas) and verified facts about the systems we work with | First as-built note or verified fact |
| `ROADMAP.md` | future | Planned work and the intended design; a part's design moves to `docs/` once it's built | First planned item beyond "Next session" |

One folder sits outside the horizons: `tools/` holds scripts for recurring
work. Check it before building, and add a script the second time you'd
re-derive the same one-off.

**The handoff** is pruned, never accumulated, and rewritten as set out in
[Closing or pausing an arc](#closing-or-pausing-an-arc):

```markdown
# <project> Handoff
<!-- one line: what this is + what to read first at session start -->

## Status (YYYY-MM-DD)
<!-- 1–3 lines: working? verified? blocked on what? -->

## Open decisions
<!-- label: question — recommendation; what turns on it. Delete when decided:
     the outcome goes to the file that owns it, the why to the commit message. -->

## Next session
<!-- the queued next steps (not "someday": that's ROADMAP) -->
```

Anything else in it is a leak: design and gotchas go to `docs/`; "what we did"
goes to git log.

**Docs describe only what exists,** so a session can trust them completely.
Design, conventions, gotchas, procedures and verified external facts go there;
list each doc in `docs/index.md`. Never document a mechanism that doesn't exist
yet.

**The roadmap holds what's planned,** clearly labeled. Prune done items.

## How we work

- **Accuracy over agreement.** Push back on wrong assumptions and flawed plans.
  Check small facts (dates, counts, file contents) with a tool. Mark unverified
  claims as unverified when you write them.
- **Lead with the answer.** Keep routine replies plain; save structure for real
  design discussion. The human's attention is the scarce resource.
- **End with only the questions the human must answer.** Decide everything else
  yourself, within the approval you hold, and say what you decided. Give each
  question a recommendation and what turns on it, and label it so either side
  can refer to it: the next letter for each reply that asks, numbered within it
  (A1, A2; then B1). A question asked again keeps its label.
- **Say what you didn't do, and why** — skipped steps, rejected fixes, dropped
  findings. Silence reads as "handled."
- **When the human asks "are you happy?"**, stop producing and evaluate the
  work against what it was meant to do. Say plainly what's wrong, missing, or
  heavier than it needs to be.
- **Link everything you point at** — files (anchored to sections), issues,
  pages. Say what an id stands for wherever you use one.
- **Fix a doc once you've confirmed it's wrong.** Wrong docs compound worse
  than missing ones.
- **One source of truth.** Write each fact once, in its most specific home, and
  link to it elsewhere. Copies drift.
- **Make the narrowest change that serves the purpose.** A line, not the file;
  a file, not the repo. Look at a file right before changing it. Undo your own
  edit by editing it back.
- **Anything that can't be undone gets its own step.** Say what it will
  delete, send or overwrite, and wait for a yes before running it.
- **Pilot before scaling.** Run anything new or large on a small sample first.
- **Give positive instructions** ("edit only `src/parser.py`") rather than
  prohibitions — for yourself and any agent you brief. A prohibition invites a
  reading that argues past it; a destination can't be reasoned around.

### Commits

- Commit only after the human agrees and the work has settled. Commit coherent
  units split by purpose, not one per edit.
- Stage only the paths you changed; others may be working in the same tree.
- Anything that can destroy work you didn't write (`reset --hard`, `clean`,
  force-push, deleting an unmerged branch) waits for the human.

### Closing or pausing an arc

An **arc** is one coherent piece of work — a task or a design — however many
turns or sessions it takes. When an arc closes, or a session ends with one
still open:

1. Fold what it taught into `docs/` (knowledge) or this file (behavior) now,
   while the context is live; a lesson deferred to a colder session is lost.
2. Rewrite `handoff.md` to the current state.
3. Commit, once the human agrees.

Steps 1–2 are part of the work and need no separate approval; step 3 does.

**Test:** if a cold session can't resume from these files, they're
under-written.

## Keeping it healthy

- **Add structure only after a real failure justifies it.** A fix is earned by
  one incident; a general rule or method waits until the pattern recurs.
- **Maintenance is subtraction.** Periodically remove what no longer serves a
  live use — especially from this file, `docs/` and `ROADMAP.md`. "Is it still
  true?" isn't the test; "what does it still do for us?" is.
- **Date verified facts.** Mark docs holding them `> Last verified: YYYY-MM-DD`;
  check the date before trusting one and re-stamp when you verify.
- **The human owns judgment and catches drift;** the AI writes and curates
  under review.

## Key facts

Add only project-specific facts that cause damage if wrong — where
secrets live, what must never be pushed or shared, deploy targets, which branch
others work from. One line each, linking to the doc that owns it.

- Credentials (switch, BMC) live in `.secrets`, which is git-ignored; never
  commit, print or paste its contents. See [`ROADMAP.md`](ROADMAP.md#components-one-static-go-binary).
- The lab switch and nodes are shared and live: switch VLAN or PoE changes and
  node power cycles need an explicit yes each time, except ports 1 and 3-8,
  whose machines are free for testing (PoE, root, wiping the OS, firmware
  updates). k53 (port 2) may be powered and used as root, never wiped. See
  [`docs/development.md`](docs/development.md#gs316ep-notes).
