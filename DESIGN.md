# chlift v1 design

Scope for v1 comes from the research and the replication soak (see `soak/`).
**v1 = Go installer + `migrate plan/start/check/cutover` + Go read-side SDK + one Grafana dashboard.**
Not in v1: dual-write, the Java/JS/C# SDKs, the LLM agent layer (we ship `--json` output plus an agent skill instead,
like pgbx), and S3 tiering.

## Layout chlift installs (soak-backed)

- **2 ClickHouse replicas, plus a 3-member Keeper:** embedded on both replicas, plus a standalone `clickhouse-keeper`
  on a third small host. chlift refuses to install with 2 Keeper members.
- **Target database:** `ENGINE = Replicated`, with the profile setting
  `database_replicated_allow_replicated_engine_arguments=2`.
  - PeerDB peer: `replicated=true`, no `cluster`, pinned to one replica.
  - No Distributed table, which avoids PeerDB #4746 and CH #97557.
  - This layout depends on the soak verdict (PASS keeps it; FAIL means single node).
- **SeaweedFS:** PeerDB's Avro staging bucket, which ClickHouse must be able to reach. The same endpoint is later used
  for clickhouse-backup. chlift adds a lifecycle rule because PeerDB never deletes its staging files (#3852).
- **PeerDB pinned to a release** (v0.37.10 tested), installed with Docker Compose on the Keeper host. Images are
  pulled unmodified (AGPLv3).
- **ClickHouse from deb/rpm LTS** (26.3) or a pinned tgz, never `curl | sh`. Uses systemd where present, otherwise a
  supervisor.
- **Prometheus `<prometheus>` endpoint (9363) switched on** for every server and Keeper.

## Postgres requirements (`chlift verify` checks; `--apply` sets them after confirmation)

- `wal_level=logical`, enough `max_replication_slots` and `max_wal_senders`, and **`max_slot_wal_keep_size` set**
  (otherwise a stalled mirror can fill the disk).
- One publication per migration.
- **`REPLICA IDENTITY FULL` on every migrated table that gets UPDATEs and has TOAST-able columns** (text, jsonb,
  bytea, arrays). In soak run 1 PeerDB silently wrote `''` for unchanged TOASTed values.
- A primary key, or `REPLICA IDENTITY FULL`.

## Commands

| command | does | done when |
|---|---|---|
| `init` | hosts, roles, Postgres DSN → `chlift.yaml` (secrets as `$VAR` refs only) | file written |
| `plan` | SSH probe (OS, RAM, disk, systemd); prints what goes where | plan printed, nothing touched |
| `install` | Keeper, ClickHouse, SeaweedFS, PeerDB, exporters; idempotent | insert on A is read on B; Keeper reports 1 leader + 2 followers |
| `verify` | Keeper quorum, replica queue, PeerDB API, S3 reachable *from ClickHouse*, Postgres checks above | all green or exact fix commands |
| `migrate plan` | reads the Postgres schema; proposes event tables, ORDER BY / PARTITION BY (fixed after creation, PeerDB #4604), engine, replica-identity fixes | DDL + mirror spec written |
| `migrate start` | creates peers + mirror over flow-api HTTP | snapshot progress shown |
| `migrate check` | per table: counts + column sums per day, per replica, `FINAL` + not deleted; then PK-range checksums | 0 mismatches |
| `migrate cutover` | records Postgres LSN W, waits until PeerDB has applied ≥ W, final `check`, stops the mirror, drops slot + publication | ClickHouse is the reader; slot gone |
| `status` | lag, slot size, mismatches, cluster health | one screen |

## Go read-side SDK (`sdk/go`)

- Modes: `postgres_only` → `shadow_read` → `clickhouse_only`.
- `EventQuery` holds two query strings per call site (PG, CH); no SQL translation in v1.
- `shadow_read` behaviour:
  - Postgres serves the request; ClickHouse runs asynchronously on a sample.
  - The comparison only covers data older than the lag watermark.
  - Results are normalised before diffing (floats, time zones, NULLs, ordering).
  - Metrics: `migration_shadow_mismatch_total` and `migration_query_latency_seconds{store}`.

## Build order after the soak verdict

1. `install` and `verify` against the soak's compose nodes. The compose becomes `compose.dev.yml`, with SSH nodes.
2. `migrate plan`, `start`, `check`, `cutover`.
3. Go SDK + dashboard.
4. Demo.
