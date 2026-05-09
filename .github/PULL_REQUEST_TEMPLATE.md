<!--
Thanks for the PR! Please tick the boxes that apply and remove the
rest. If your change is bigger than a typo, link the issue it closes.
-->

## What

<!-- One sentence summary of the change. -->

## Why

<!-- The "why" matters more than the "what" — why does this exist? -->

Closes #

## Checklist

- [ ] `go vet ./...` passes
- [ ] `go test ./... -race -count=1` passes locally
- [ ] `golangci-lint run` passes locally (or it was already failing on `main`)
- [ ] `CHANGELOG.md` updated under the right `[unreleased]` / version header
- [ ] README / `--help` output updated if user-visible behaviour changed
- [ ] No new `TODO` / `FIXME` / `panic("not implemented")` introduced
- [ ] No new dependency, OR a one-line note below explains why one was needed

## Screenshots / asciinema (optional)

<!-- For UI-affecting changes (doctor output, dashboard, etc.) -->
