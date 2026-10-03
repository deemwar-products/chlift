# Contributing to chlift

Thanks for helping. A few rules keep chlift trustworthy.

- **Evidence over claims.** A change that affects data movement must come with a run that proves it: a test, or the
  output of `chlift migrate check --checksums` on the dev environment (`dev/compose.dev.yml`).
- **Never weaken a safety check silently.** Examples: `REPLICA IDENTITY FULL` enforcement, one pinned ClickHouse build,
  the cutover gate.
- **Secrets never appear on command lines, in logs, or in errors.** Pass them on stdin or in 0600 files.
- **Before a PR, run:** `gofmt -l .`, `go vet ./...`, `go test ./...`.

**Dev environment:** `dev/compose.dev.yml` starts three SSH nodes and a Postgres source. Then run `chlift init` →
`install` → `chlift seed` → `migrate plan/start/check`. The exact commands are in `docs/evidence/steps/`.

By contributing you agree your contribution is licensed under the MIT License.
