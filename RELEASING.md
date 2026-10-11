# Releasing

Pushing a `v*` tag runs [`.github/workflows/release.yml`](.github/workflows/release.yml):
it tests, then [goreleaser](.goreleaser.yaml) builds darwin, linux and windows
binaries for amd64 and arm64, publishes them with `checksums.txt` as a GitHub
release, and updates the Homebrew tap. Tags with a pre-release suffix
(`v0.2.0-rc.1`) become pre-releases.

```bash
git tag v0.1.0
git push origin v0.1.0
```

## One-time setup for the Homebrew tap

1. Create the public repository `CoinkWang/homebrew-tap` (it may be empty).
2. Create a fine-grained token with **Contents: read and write** on that
   repository only.
3. Add it to this repository as the Actions secret `HOMEBREW_TAP_GITHUB_TOKEN`.

Without the secret the release still publishes binaries and skips the tap.

## Check the configuration locally

```bash
goreleaser check
goreleaser release --snapshot --clean   # builds into dist/, publishes nothing
```
