# Contributing to a2abridge

Thanks for considering a contribution! This project is small enough
that contributing is genuinely low-friction.

## Quick rules

1. **Open an issue first** for anything beyond a typo / one-line fix —
   it's frustrating for both of us if you spend an evening on a PR
   that turns out to clash with planned work.
2. **One logical change per PR**. A test fix + a feature add = two PRs.
3. **Tests required** for new behaviour. We're at 44 cases under
   `-race`; let's keep that line going up.
4. **Keep the docs current** — README sections, `CHANGELOG.md` entry,
   `--help` output — all in the same PR as the code change.

## Local setup

```bash
git clone https://github.com/vbcherepanov/a2abridge
cd a2abridge
go test ./... -race -count=1     # must be green before any work
go build ./cmd/a2abridge         # smoke
./a2abridge doctor               # check the local environment
```

## What we look for in a PR

- `go vet ./...` clean.
- `go test ./... -race` green on macOS/Linux. Windows-specific tests
  skip via `runtime.GOOS == "windows"`.
- `golangci-lint run` — the same config CI uses.
- Imports go: stdlib block, then third-party, then `github.com/vbcherepanov/a2abridge/...`.
- No new TODO / FIXME / panic("not implemented"). If a thing isn't
  done, file an issue and pull-request the partial work behind a
  feature flag, not behind a TODO.
- No new dependency unless absolutely necessary (and document why).

## Commit style

Conventional Commits. Examples:

```
feat(cli): add `a2abridge worker --restart-on-crash` flag
fix(security): accept colon-separated A2A_TRUST_ROOTS on Windows too
test(a2a): cover -32007 VersionNotSupported branch
docs(integrations): add Semantic Kernel example
```

Keep the subject under 72 characters. Use the body for the **why**.
Don't add `Co-Authored-By` lines for AI assistants — commits should
look hand-written.

## Areas where we'd love help

- **gRPC binding (§7.2)** — see `roadmap` in README. Has open
  scaffolding but no `.proto` toolchain in CI yet.
- **Web dashboard** — a tiny `/dashboard` HTML on the directory daemon
  to visualise peers + tasks live.
- **More integration guides** — Semantic Kernel, AutoGen, BeeAI
  Framework. Format is in `docs/integrations/`.
- **Translations of the README** — Russian, Chinese, Japanese.
- **Real-world cookbook recipes** — drop them under `docs/recipes/`.

## Releasing (maintainers only)

```bash
# 1. Update CHANGELOG.md
# 2. Tag and push:
git tag -a vX.Y.Z -m "vX.Y.Z — short summary"
git push origin vX.Y.Z
# 3. release.yml builds + uploads artefacts automatically.
# 4. gh release edit vX.Y.Z --latest    # mark as Latest in UI
```
