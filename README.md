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
root. A typical file:

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
cargo, enabled with `preset = "auto"` or a preset name under `[worktree]`;
`bothways preset show` prints the `[worktree]` settings in effect.

### Reference

Every key, with its default:

```toml
[worktree]
root        = ""        # where worktrees go; default $XDG_CACHE_HOME/bothways/<repo-id>.
                        # Relative paths are resolved against the repository root; "~/" works.
preset      = "none"    # "auto" picks a built-in preset from the lockfiles, or name one
setup       = ""        # shell command run in every new worktree, and again by `up` when
                        # the fingerprint files changed since the last successful run
fingerprint = []        # files whose content decides whether setup reruns; empty = always rerun
max_disk    = ""        # e.g. "20GB": `gc` removes idle worktrees until the root fits

[worktree.paths]        # gitignored paths a new worktree takes from the base's worktree:
                        #   clone  copy-on-write copy (APFS, btrfs, xfs); plain copy elsewhere
                        #   link   symlink to the base worktree's copy
                        #   copy   plain copy
                        #   fresh  leave it out for setup to rebuild (the default)

[preview]               # set start or deploy, not both
start          = ""     # long-running command bothways supervises, e.g. "pnpm dev --port {{.Port}}"
url            = "http://localhost:{{.Port}}"   # address of a start-mode preview
health         = ""     # path appended to url and polled until it answers 200, e.g. "/"
timeout        = "60s"  # how long health polling waits
deploy         = ""     # one-shot script; the last non-empty line it prints is the URL
down           = ""     # tears down what deploy created; without it `down` only forgets the URL
deploy_timeout = "15m"
slots          = 4      # previews running at once
base_port      = 3000   # {{.Port}} is base_port + {{.Slot}}

[preview.env]           # extra environment for start, deploy and down, e.g.
                        # DATABASE_URL = "postgres://localhost/app_{{.Slot}}"

[fork]
similarity_threshold = 0.6   # fork refuses a question this similar to an open one (0 to 1);
                             # `fork --force` skips the check

[run]
runner = ""             # agent command `bothways run` starts in each variant's worktree, e.g.
                        # "claude -p \"$(cat '{{.PromptFile}}')\""
jobs   = 2              # runners at once
```

`start`, `url`, `deploy`, `down` and the `[preview.env]` values are Go templates
over `{{.Slot}}`, `{{.Port}}`, `{{.Name}}` (`shape-circle`), `{{.Decision}}`,
`{{.Variant}}` and `{{.Worktree}}`. A slot belongs to a running preview, not to a
variant, so a variant may get a different one next time; name remote
environments after `{{.Name}}`. The `runner` template gets `{{.Worktree}}`,
`{{.Decision}}`, `{{.Variant}}` and `{{.PromptFile}}`.

Every command bothways runs gets `BOTHWAYS_NAME`, `BOTHWAYS_DECISION`,
`BOTHWAYS_VARIANT` and `BOTHWAYS_WORKTREE` in its environment; previews also get
`BOTHWAYS_SLOT`. Setup runs before a slot is assigned, so it has no slot and is
not a template.

The same keys in `~/.config/bothways/config.toml` (or under `$XDG_CONFIG_HOME`)
set per-user defaults, such as a `runner` or a worktree `root`. The repository
file wins key by key, and only its commands need `bothways trust`.

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

[`SKILL.md`](plugins/bothways/skills/bothways/SKILL.md) teaches an agent when to
fork, how to work in the worktrees, and how to hand the decision back. The agent
still needs the `bothways` CLI installed.

In Claude Code, install it as a plugin, then invoke it with `/bothways:bothways`:

```
/plugin marketplace add CoinkWang/bothways
/plugin install bothways@bothways
```

Or install the copy embedded in the binary, which always matches the installed
CLI; rerun it with `--force` after upgrading bothways:

```bash
bothways skill install                       # ~/.claude/skills/bothways/SKILL.md, invoked as /bothways
bothways skill install --dir <skills dir>    # any other agent that reads Agent Skills
```

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
| `skill` | Print or install the agent skill matching this binary |

`bothways <command> --help` has the details.

## Limitations

- Works on whole git repositories; no monorepo subdirectories, no jj.
- Previews assume the project can be started with one command.
- `combo` worktrees cannot be previewed with `up` yet.
- Windows is experimental: CI builds and tests it, but does not yet require it
  to pass.

## License

[MIT](LICENSE)
