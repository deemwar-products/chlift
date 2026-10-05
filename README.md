# chlift

> **Replication soak: passed.** 20.4 hours of injected failures (28 replica, worker and Keeper faults), no lost or corrupted
> rows. Replicated targets in the layout chlift creates are no longer preview. Where ClickHouse lagged and why, and the
> hardware it needs: [soak results](docs/evidence/numbers.md#replication-soak-2026-10-03-to-04).

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

`chlift migrate exporter` serves Prometheus metrics for a running migration: mirror state, replication-slot WAL, rows
in Postgres vs each replica, and the per-day check (run only once the mirror has been RUNNING for 2 minutes). A
Grafana dashboard is in `deploy/grafana/`.

`--json` gives machine-readable output on every command. Secrets are generated into a 0600 file and never printed.

## Switching reads safely: the Go SDK

`sdk/go` moves an application's analytics reads in three stages: `postgres_only`, then `shadow_read`, then
`clickhouse_only`, set with `CHLIFT_MODE`.
- In `shadow_read`, Postgres answers every request. A sample of reads also runs on ClickHouse in the background, and
  the two results are normalised (time zones, number formats, row order) and compared. Mismatches and latency are
  counted, and never returned to the caller.
- Each call site carries both SQL strings, because the dialects differ. chlift doesn't translate SQL.
- Shadow-read **settled data**. ClickHouse is read a moment after Postgres, and CDC applies changes a few seconds
  later. So a query that includes rows written in the last seconds (this week so far, all-time totals) can differ by a
  few rows on a busy table, either way. That's a timing difference, not a wrong translation. End the compared window
  before now (for example, exclude today). In our live-write test, the query on completed days matched every time;
  the two that included current rows did not.
- Example: on our dev data, three correct translations matched. A wrong one was caught: ClickHouse's `toStartOfWeek`
  starts weeks on Sunday, Postgres's `date_trunc('week')` on Monday. See `sdk/go/example`.

## What it requires

- Hosts: Debian/Ubuntu (tested) or RHEL-family (tested on Rocky Linux 9 without systemd; RHEL with systemd not yet
  tested), SSH key access, and Docker with compose v2 on the PeerDB host.
- Postgres 12+ with `wal_level=logical`. Set `max_slot_wal_keep_size`, so a stalled mirror can't fill the disk.
- ClickHouse from the official LTS packages, the same pinned build on every host.
- **Memory per ClickHouse replica: 16 GB minimum, 32 GB+ for production** (ClickHouse's own guidance). The soak ran at
  1.5 GB per replica and hit ClickHouse memory-limit errors (Code 241) on its verification queries once the tables held
  a few million rows. Sizes between 1.5 GB and 16 GB were not tested.
- **CPU: plan for more than 2 cores per mirror.** Measured: one mirror (PeerDB plus the two replicas it writes into),
  limited to 2 cores' worth of CPU by a quota on shared cores, kept every row for 6 hours under injected faults. But in
  1 of 36 cycles, it fell behind the 5-minute freshness window while running at that CPU cap, and caught up exactly
  by the next check. That was at about 61 inserted rows/s, with steady updates and deletes. **3 cores per mirror is
  our derived estimate, not yet confirmed.** Add cores in proportion to your write rate. Details:
  [confirm run](docs/evidence/numbers.md#confirm-run-2026-10-04-to-05).

## Amazon RDS / Aurora PostgreSQL

Tested on RDS PostgreSQL 16 with the master user: snapshot, CDC and `check --checksums` all passed (0 of 64 buckets
differ).
- Enable `rds.logical_replication = 1` in the DB parameter group and reboot.
- Set `max_slot_wal_keep_size` in the same parameter group. `ALTER SYSTEM` is not allowed on RDS.
- The master user can run `migrate start --apply-postgres` (REPLICA IDENTITY FULL, CREATE PUBLICATION) with no extra
  grants.
- *Not yet tested:* a non-master user needs the `rds_replication` role, and must own the migrated tables for
  `REPLICA IDENTITY FULL`.

## Limits today

- PeerDB's own ClickHouse cluster mode (with or without quorum writes) is not what chlift uses. It stays untested for
  production: in the soak it lagged past 5 minutes at normal load.
- One Postgres table maps to one ClickHouse table. No automatic denormalisation of joins.
- Sort key and partitioning can't change after the first snapshot (a PeerDB limit). Review the plan before
  `migrate start`.
- The Go read-side SDK (`sdk/go`) is new in this release; SDKs for other languages are not written yet.

## Evidence

Every number we publish is in [`docs/evidence/numbers.md`](docs/evidence/numbers.md), next to the raw logs, the
terminal recordings (asciinema) and its caveats. The exact commands that produced them are in
[`docs/evidence/steps/`](docs/evidence/steps/).

## License

MIT. See [LICENSE](LICENSE). PeerDB, ClickHouse and SeaweedFS are separate projects under their own licenses; chlift
runs their official, unmodified releases.
