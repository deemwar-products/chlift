# chlift: numbers we publish, and where each one comes from

Everything below comes from one recorded run on 2026-10-03 (UTC), starting from empty nodes and an empty
ClickHouse. The terminal recordings are in `casts/` (asciinema v2, real timing). Each `.log` beside a cast is the
same output as plain text. The commands are in `steps/`: each step script prints a command and then runs it. Nothing
on screen was typed or edited by hand.

**Environment (applies to every number):** one shared 8-core Linux server.
- **ClickHouse:** two replicas plus a standalone Keeper, each in its own container (ClickHouse 26.3.40.16; replicas
  capped at 1.5 GB RAM each).
- **PeerDB and SeaweedFS:** on the same server.
- **Postgres:** 16, default settings, in a container capped at 768 MB.
- **Data:** synthetic demo data from `chlift seed` (six months of timestamps).
- Other workloads were running on the server at the same time.

**This is not a vendor benchmark.**

| # | Figure | Source | Caveat |
|---|---|---|---|
| 1 | Demo database: user_events ~8.0M, request_logs ~4.0M, system_logs 2.0M rows; 2.6 GB | `casts/00-seed.log` | Synthetic data. The row figures are Postgres estimates (`n_live_tup`). |
| 2 | Install from empty nodes, 3 hosts + PeerDB, all checks ok: **2 min 22 s** | `casts/01-install.log` (`real 2m22.437s`) | Hosts are containers on one server; packages come from the official ClickHouse repo. |
| 3 | `migrate plan` picks the 3 event tables, keeps users/accounts in Postgres, and requires `REPLICA IDENTITY FULL` on all three | `casts/02-migrate-plan.log` | |
| 4 | **14,000,000 rows** (8,000,000 + 4,000,000 + 2,000,000) copied to **both** ClickHouse replicas in **≤ 2 min 26 s** | `casts/03-migrate-start.log`: mirror created; `date` 16:37:45; status RUNNING with equal counts; `date` 16:40:11 | Upper bound: status was polled every 15 s. Replica counts in `status` are physical rows. |
| 5 | Live CDC: 50,000 inserts, 1,000 updates, 5,000 deletes; status 56,000 rows synced by CDC after 45 s | `casts/04-cdc.log` | `status` replica counts include update and delete versions (physical rows). |
| 6 | After CDC, `migrate check --checksums`: **every day equal** in Postgres and on **both** replicas (8,047,000 / 4,000,000 / 1,998,000 live rows), and **0 of 64** primary-key-range checksum buckets differ per table | `casts/05-migrate-check.log` | The checksums compare counts, ids, integers, timestamps and text lengths per bucket. JSON and float columns are not compared. |
| 7 | **Without** `REPLICA IDENTITY FULL` (stripped on purpose): after 4,000 updates and 1,000 deletes, Postgres has 19,000 rows and **both** replicas 20,000; long texts arrive empty (text bytes in bucket 1298..1594: 124,215 in Postgres vs 1,335 in ClickHouse); no error anywhere until `migrate check` fails | `casts/07-corruption-repro.log`, first half | A deliberate bypass of chlift's own requirement, to show the failure. The first bucket's label and count (`1001..1297`, 593) come from a bucketing artifact, fixed after this recording; the verdict does not change. |
| 8 | **With** the requirement chlift enforces, same kind of changes: 18,000 = 18,000 = 18,000, **0 of 64** buckets differ | `casts/07-corruption-repro.log`, second half | |
| 9 | Query time, **while PeerDB still replicates** (ClickHouse queries use `FINAL`): **14× to 163×** faster than Postgres | `bench-raw.json` + `casts/06-bench.log`; recompute with `python3 bench_table.py bench-raw.json` | Median of 5 runs per query, all taken in one invocation. Postgres time is measured from the client and includes sending the result; ClickHouse time is the server's own elapsed time. A tuned Postgres on bigger hardware would narrow the gap. |
| 10 | `migrate cutover`: watermark confirmed by PeerDB, then day counts and checksums equal on every replica (**0 of 64 buckets differ** on all 3 tables); the mirror stops and ClickHouse keeps the data; `check` still matches afterwards. Total **1 min 52 s** | `casts/08-cutover.log` (`real 1m51.951s`) | Recorded with a later build that fixes checksum bucketing (rows below Postgres's lowest id are now compared too). The one replication slot and publication still listed at the end of that cast belong to the demo mirror from cast 07, not to this migration: `casts/09-cleanup.log` shows them by name, removes them, and ends with 0 slots and 0 publications. |
| 11 | Query time **after cutover** (or for append-only tables; no `FINAL`): **31× to 308×** faster | same as 9 | Same caveats. Cutover was not part of this recording. |

Per query (median of 5 runs, ms; Postgres / ClickHouse FINAL / ClickHouse):

| Query | Postgres | ClickHouse FINAL | ClickHouse |
|---|---|---|---|
| Daily active users, last 30 days | 5,388 | 266 | 107 |
| Events per kind per week, 6 months | 21,950 | 1,600 | 710 |
| Top 10 accounts by events, last 7 days | 5,740 | 159 | 57 |
| p95 latency per endpoint, last 30 days | 23,994 | 147 | 78 |

**Bugs this recording found (all fixed in this release):**
- On a fresh PeerDB the Temporal search attribute raced the first mirror create.
- The step script's "snapshot finished" test was wrong.
- Checksum bucketing skipped ClickHouse-only rows below Postgres's lowest id.
- Every mirror shared one pair of PeerDB peers, so a new mirror retargeted the older mirrors' ClickHouse database. That
  left the cast-07 demo mirror stuck in TERMINATING until its peer was pointed back by hand. Peers are now per mirror,
  and abort/cutover drop them.

**Not claimed:** the PeerDB issue #4746 (row loss with a Distributed target) has not been reproduced by us. chlift
avoids that layout. The replication soak under faults is still running; until it passes, replicated targets are
"preview".
