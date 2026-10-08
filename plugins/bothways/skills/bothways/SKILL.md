---
name: bothways
description: Build several approaches to one task side by side in separate git worktrees, run them, then keep one with a fast-forward.
disable-model-invocation: true
---

# bothways

Several ways of doing one task, kept as sibling branches on a shared base, one worktree each. `pick` fast-forwards the base to the one the human chose; the others never entered the base's history, so nothing needs reverting.

Three names: a **decision** is the question (`shape`), a **variant** is one answer (`circle`), the **base** is the branch the user was already on. A variant is always written `shape=circle`.

This skill drives the `bothways` CLI. If `bothways --version` fails, tell the user it is not installed and how to get it — `brew install CoinkWang/tap/bothways`, or `go install github.com/CoinkWang/bothways/cmd/bothways@latest` — instead of installing it yourself.

## Authorization

This skill being loaded authorizes nothing by itself. Every command that writes runs only after the user says yes to that specific command in this conversation. These run freely: `status`, `tree`, `path`, `diff` (all with `--json`), `note`, `compare --no-open`, and `trust --show`.

One yes covers one command, not the session and not the next decision. A second decision needs a second yes.

State the side effects when you propose, because they land outside the diff the user is watching:

- `fork` creates one branch `bothways/<decision>/<variant>` and one worktree under `~/.cache/bothways/` per `--alt`.
- `fork --carry <v>` **moves the uncommitted changes out of the user's working tree** into that variant's worktree.
- `fork --split <rev>` **rewinds the user's base branch to `<rev>`** and moves `<rev>..HEAD` onto the first `--alt`. Quote the resolved rev and the commits it will move before asking.
- `hoist <commit>` **adds a commit to the user's base branch** and removes it from the variant.
- `pick` moves the base branch ref, deletes the picked variant's branch, stops previews, and removes the sibling worktrees.
- `drop`, `gc`, and `--purge` delete branches and worktrees; `--force` discards uncommitted files in them.
- `up` starts a dev server per variant, or runs the repository's deploy script — which may create remote environments that cost money. `down` stops or tears them down.
- `run` starts an agent runner in every target variant's worktree.
- `compare` without `--no-open` opens the user's browser.
- `trust` lets `.bothways.toml` run its commands — setup, preview start, deploy and down scripts, the runner — for this user until the file changes. Run `trust --show` first and quote every command it lists; never run `trust` just to get past an error.
- `init --hooks` installs a persistent post-commit hook. Say plainly: from then on, **every commit on the base branch syncs the variants in the background**, and that one yes covers all of those syncs. `init --no-hooks` removes it.

## When to fork

Fork only when the user asks for more than one implementation of the same thing: "build both", "make A and B so I can compare", a spec that lists alternatives to look at side by side. Take the question and the alternatives from their words; if they asked for "a few versions" without naming them, name two and say what each does. The request is what makes a fork the right move, not the yes for the command: still give the exact `fork` command and wait.

A divergence you find on your own is never a reason to fork, or to propose one. Route it as you would without this skill:

- **Decide yourself** when the tradeoff has a defensible answer — naming, data structure, where a helper lives. Choose, and give one line of why.
- **Ask** when the choice is the user's — visual treatment, copy, interaction feel — or a fact only they have is missing. Ask in plain words. If they answer that they want both, that answer is the request.

Keep at most three decisions open at once.

## Fork

The parent worktree must be clean. Three entry points:

```bash
# Neither approach written yet.
bothways fork shape --alt circle --alt square --q "round or square logo frame?"

# One approach already written, still uncommitted.
bothways fork shape --alt circle --alt square --carry circle

# One approach already committed: <rev>..HEAD becomes the first --alt, base rewinds to <rev>.
bothways fork shape --alt circle --alt square --split <rev>
```

ids are short English words: they become branch names, directory names and preview names. The first `--alt` is the default variant; the default only decides whose nested decisions get worktrees, and nothing is ever picked automatically. fork prints one `shape=circle<TAB>/abs/path` line per variant; `bothways path shape=circle` prints one path later.

After `--carry`, the carried work arrives **uncommitted** in that variant's worktree. Commit it first — otherwise `sync` re-applies it over a moved base and conflicts.

## Work in the worktrees

`cd` into a variant's worktree and use plain git. Committing inside a variant worktree is ordinary work on a branch the user already approved, so it needs no further yes. One rule places every change:

- Only this variant needs it → commit in the variant's worktree.
- Both variants need it (a bug fix, a shared helper) → commit in the **base** worktree, then propose `bothways sync` — unless the hook is installed (`status --json` → `hooks.post_commit`), in which case the sync has already started.
- A shared change already committed in a variant → propose `bothways hoist <commit>`.

Shared work living in the base is what keeps the comparison fair and `pick` a fast-forward.

## Hand the decision back

The choice belongs to the human. Once every variant is built, propose `bothways up` then `bothways compare <decision>`, which opens every variant's running preview side by side. Then reply with:

- the question,
- one line per variant on what it actually does,
- the preview URL of each (from `status`), or its worktree path when the repository has no `[preview]` section,
- the exact command for each answer: `bothways pick shape=circle`.

Their answer to that question is the yes for that `pick`, and for nothing else.

## Converge

```bash
bothways pick shape=circle   # base fast-forwards to circle; square moves to bothways/rejected/
bothways drop shape=square   # remove one variant without deciding
```

`pick` syncs first, moves the base ref, reclaims the other worktrees, and re-syncs the remaining decisions. Rejected branches survive under `bothways/rejected/` until `bothways gc` (14 days by default), so a reversed decision is recoverable — which is also why `gc` and `--purge` are worth a separate yes.

## When the base moves

`bothways sync` restacks every variant onto the latest base tip. A conflict stops only that variant:

```
color=blue  conflict  resolve in /…/color/blue, then `git rebase --continue`
```

Resolve it in that worktree; the other variants keep working. `sync --skip-dirty` leaves variants with uncommitted or untracked changes alone and reports them `skipped` — the hook always syncs this way, so a variant you are editing may lag until you commit and sync.

Only one command that rewrites branches or worktrees runs per repository at a time. `bothways: waiting for … to finish` means another one — often a hook-triggered sync — holds the lock; it proceeds on its own.

## Reading results

`bothways status --json` is the machine-readable view of every decision and variant. Its shape is `schema/state.schema.json` in the bothways repository, whose descriptions define every `state` and `preview.state` value. Check `schema_version` and stop if it is not the version you expect; ignore fields you do not recognize, since fields are only ever added.

- `behind_base` > 0 → the variant needs `sync`.
- `state: "conflict"` → finish the rebase in that variant's `worktree`.
- `state: "reclaimed"` → gc removed the worktree to save disk; `sync` or `up` rebuilds it. `worktree` is empty meanwhile, as it is for `deferred`.
- `combos` are read-only merges, not variants: never pass a combo name to `sync`, `pick` or `drop`.

Until `.bothways.toml` is trusted, `fork`, `add` and `sync` create worktrees but print `setup skipped: … is not trusted`, and `up`, `restart`, `run` and `down` exit 1 naming the file. Both mean the same thing: show the user `trust --show` and propose `bothways trust`. `trust --show --json` → `state` is `trusted`, `untrusted`, `changed` (edited since it was trusted) or `not-needed`.

Exit codes are the contract: 0 ok, 1 user error, 2 conflict or partial failure, 3 environment error — branch on the code rather than on the message text. `bothways doctor` rebuilds `state.json` from the `bothways/*` branches and removes entries it cannot match, so propose it as a repair, not as a probe.
