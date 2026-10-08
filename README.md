# bothways

Build several approaches to the same task side by side, each in its own git
worktree on top of a shared base branch, run them, and keep one with a
fast-forward. Rejected variants never enter the base's history, so there is
nothing to revert.

> Status: pre-release (v0.1.0). Commands, `.bothways.toml` and the
> `status --json` output may still change.

## Why

A coding agent can write two versions of a change in minutes, but choosing
between them from a description is guesswork, and switching after the fact
means reverts and cherry-picks. bothways keeps every version checked out and
running, so the choice is made by looking at the result, and the cost of the
losing version is deleting a directory.

## Install

Requires git 2.38+.

```bash
brew install CoinkWang/tap/bothways
```

Or download a binary for macOS, Linux or Windows from
[Releases](https://github.com/CoinkWang/bothways/releases), or build it with
Go 1.24+:

```bash
go install github.com/CoinkWang/bothways/cmd/bothways@latest
```

`bothways --version` prints the installed version.

## Concepts

- **base**: the branch you are working on. bothways never creates it; `pick`
  fast-forwards it.
- **decision**: a named question, such as `shape`.
- **variant**: one answer to it, such as `circle`, written `shape=circle`. Each
  variant is a branch `bothways/shape/circle` stacked on the base, with its own
  worktree under `~/.cache/bothways/`.

Work that every variant needs goes on the base; work that belongs to one
variant is committed in its worktree. That keeps each variant a fast-forward
of the base.

## Quick start

```bash
# Open a decision: one branch and one worktree per variant.
bothways fork shape --alt circle --alt square --q "round or square logo frame?"

# Work in a variant's worktree with plain git.
cd "$(bothways path shape=circle)"

# See every variant and how far behind the base it is.
bothways status

# The base moved on? Restack every variant onto it.
bothways sync

# Run every variant and compare them side by side in the browser.
bothways up
bothways compare shape

# Decide: the base fast-forwards to circle, square moves to bothways/rejected/.
bothways pick shape=circle
```

Already wrote one approach? `fork --carry circle` moves uncommitted changes into
`circle`; `fork --split <rev>` turns the commits `<rev>..HEAD` into it. A commit
every variant needs landed in one of them? `bothways hoist <commit>` moves it
down to the base.

## Configuration

Previews and worktree setup are configured in `.bothways.toml` at the repository
root:

```toml
[worktree]
setup       = "pnpm install --frozen-lockfile"   # skipped while the fingerprint files are unchanged
fingerprint = ["pnpm-lock.yaml"]

[worktree.paths]                                 # gitignored paths: clone, link, copy or fresh
".env" = "copy"

[preview]
start  = "pnpm dev --port {{.Port}}"             # or: deploy = "./scripts/deploy.sh {{.Name}}"
health = "/"
slots  = 4
```

`bothways preset list` shows built-in presets for pnpm, npm, bun, uv, pip and
cargo, enabled with `preset = "auto"` or a preset name under `[worktree]`.

## Trust

`.bothways.toml` can make bothways run shell commands. None of them runs until
you have reviewed the file and trusted it:

```bash
bothways trust --show   # what the file runs, and whether it is trusted
bothways trust          # trust the file as it is now
bothways trust --revoke
```

Trust is recorded per user in `$XDG_STATE_HOME/bothways/trust.json`, never in
the repository, and covers the file's exact content: any edit needs a new
trust. Until then, `fork`, `add` and `sync` create worktrees without running
setup, and `up`, `restart`, `run` and down scripts refuse to start.

## Using it with coding agents

[`SKILL.md`](SKILL.md) teaches an agent when to fork, how to work in the
worktrees, and how to hand the decision back. In Claude Code, copy it to
`~/.claude/skills/bothways/SKILL.md` and invoke it with `/bothways`.

Every read command accepts `--json`. The shape of `bothways status --json` is
[`schema/state.schema.json`](schema/state.schema.json). Exit codes: `0` ok,
`1` user error, `2` conflict or partial failure, `3` environment error.

## Commands

| Command | What it does |
|---|---|
| `init [--hooks]` | Prepare the repository; `--hooks` restacks variants after every commit on the base |
| `fork`, `add` | Open a decision, or add variants to one |
| `status`, `tree`, `path`, `diff` | Inspect variants |
| `sync` | Restack variants onto the latest base |
| `pick`, `drop` | Keep one variant, or remove one without deciding |
| `hoist` | Move a commit from a variant down to the base |
| `up`, `down`, `restart`, `compare` | Run previews and look at them side by side |
| `combo` | Read-only worktree merging variants of different decisions |
| `run` | Start an agent runner in each variant's worktree |
| `note` | Attach a note shown in `status` |
| `gc`, `doctor` | Clean up rejected branches and worktrees, rebuild state |
| `trust`, `preset` | Allow the repository's commands, inspect presets |

`bothways <command> --help` has the details.

## Limitations

- Works on whole git repositories; no monorepo subdirectories, no jj.
- Previews assume the project can be started with one command.
- `combo` worktrees cannot be previewed with `up` yet.
- Windows is experimental: CI builds and tests it, but does not yet require it
  to pass.

## License

[MIT](LICENSE)
