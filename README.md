# chlift

> **Preview.** The replication soak (PeerDB into a 2-replica ClickHouse cluster under faults) is still running. Until it
> passes, treat replicated targets as preview. The results so far are in [`docs/evidence/`](docs/evidence/).

**Move your Postgres event tables into a self-hosted, replicated ClickHouse cluster, and prove nothing was lost.**

chlift is for teams running their own Postgres whose events tables (user activity, request logs, audit trails) have
grown to tens of GB, and whose analytics queries now take seconds to minutes. It does three things:

1. **Installs a ClickHouse cluster over SSH:** two or more replicas, a 3-member Keeper, PeerDB for change data capture,
   and a SeaweedFS staging store. Plain Linux hosts, no Kubernetes, no cloud service.
2. **Plans and runs the migration:** it picks the event tables, proposes sort keys and partitions, applies the Postgres
   settings that keep the copy correct, then runs a snapshot plus live CDC.
3. **Proves the copy:** it compares row counts for every day and column checksums over primary-key ranges, on **every
   replica**, before you cut over.

chlift is free and MIT licensed. The only paid offers are installation, support and training: io@deemwar.com.

## Why the proof matters

We found two ways a Postgres → ClickHouse replication setup **silently** produces wrong data. In both cases the
pipeline reports success and prints no error:

| What happens | Why | What chlift does |
|---|---|---|
| Long text/JSON values arrive **empty** after an UPDATE that didn't touch them | Postgres doesn't log unchanged TOASTed values unless the table uses `REPLICA IDENTITY FULL` | `migrate plan` requires `REPLICA IDENTITY FULL` for updated tables with TOAST-able columns |
| **Deleted rows stay visible** in ClickHouse | A DELETE carries only the key; with a time-first sort key the delete marker lands under a different key and never folds the row | `migrate plan` requires `REPLICA IDENTITY FULL` when the sort key doesn't start with the primary key |

Both were reproduced and caught by `chlift migrate check`; see the recording `07-corruption-repro` in
[`docs/evidence/`](docs/evidence/).

## Quick start

```sh
# Download the release binary (linux amd64 or arm64) and verify it against SHA256SUMS.
chlift init --cluster prod \
  --host a=10.0.0.1:clickhouse,keeper --host b=10.0.0.2:clickhouse,keeper --host c=10.0.0.3:keeper,peerdb
chlift plan                      # probes every host; changes nothing
chlift install                   # idempotent: re-running changes nothing unless the config changed
chlift verify                    # Keeper quorum, replica health, a row written on one replica and read on all

export CHLIFT_PG_DSN='postgres://USER:YOUR_PASSWORD_HERE@db:5432/app'
chlift migrate plan              # writes chlift-migration.yaml for you to review
chlift migrate start --apply-postgres
chlift migrate status
chlift migrate check --checksums # every day and every PK range, on every replica
chlift migrate cutover           # watermark, final proof, stop CDC, drop the slot; ClickHouse keeps the data
```

`--json` gives machine-readable output on every command. Secrets are generated into a 0600 file and never printed.

## Switching reads safely: the Go SDK

`sdk/go` moves an application's analytics reads in three stages: `postgres_only`, then `shadow_read`, then
`clickhouse_only`, set with `CHLIFT_MODE`.
- In `shadow_read`, Postgres answers every request. A sample of reads also runs on ClickHouse in the background, and
  the two results are normalised (time zones, number formats, row order) and compared. Mismatches and latency are
  counted, and never returned to the caller.
- Each call site carries both SQL strings, because the dialects differ. chlift doesn't translate SQL.
- Example: on our dev data, three correct translations matched. A wrong one was caught: ClickHouse's `toStartOfWeek`
  starts weeks on Sunday, Postgres's `date_trunc('week')` on Monday. See `sdk/go/example`.

## What it requires

- Hosts: Debian/Ubuntu (tested) or RHEL-family (written, not yet tested), SSH key access, and Docker with compose v2
  on the PeerDB host.
- Postgres 12+ with `wal_level=logical`. Set `max_slot_wal_keep_size`, so a stalled mirror can't fill the disk.
- ClickHouse from the official LTS packages, the same pinned build on every host.

## Limits today

- **Preview:** replicated targets wait on the soak verdict.
- One Postgres table maps to one ClickHouse table. No automatic denormalisation of joins.
- Sort key and partitioning can't change after the first snapshot (a PeerDB limit). Review the plan before
  `migrate start`.
- Managed Postgres (RDS/Aurora) is not yet tested.
- The Go read-side SDK (`sdk/go`) is new in this release; SDKs for other languages are not written yet.

## Evidence

Every number we publish is in [`docs/evidence/numbers.md`](docs/evidence/numbers.md), next to the raw logs, the
terminal recordings (asciinema) and its caveats. The exact commands that produced them are in
[`docs/evidence/steps/`](docs/evidence/steps/).

## License

MIT. See [LICENSE](LICENSE). PeerDB, ClickHouse and SeaweedFS are separate projects under their own licenses; chlift
runs their official, unmodified releases.
