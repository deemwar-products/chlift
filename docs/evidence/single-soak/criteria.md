<!-- Published copy of the criteria committed BEFORE the run (2026-10-05 07:22 IST). Only internal reviewer names
     were replaced by neutral roles (4 phrases); nothing else changed. sha256 of the original: 2e3e2c095a5235d213438b30de4f11aa9bacfc419e5d66fa1bb1bbf6c2391035 -->
# chlift single-server fault soak: pass/fail criteria (fixed BEFORE the run)

**Purpose:** the gate for a single-server-scoped chlift v1.0.0 (2026-10-05). A failed gate means no v1.0.0
tag. Written and committed on 2026-10-05 around 01:50 UTC; the run starts at or after 17:30 UTC (23:00 IST) the same
day.

## What runs
- **Topology:** Postgres 16 → ONE ClickHouse 26.3.39.7 with an embedded single-member Keeper → PeerDB v0.37.10.
- **ClickHouse database:** what `chlift migrate start` creates on one host: a `Replicated` database with one replica.
- **Honest difference from the AMI:** these are containers from chlift's soak harness (`soak/compose.yml` +
  `soak/single.override.yml`), not the installer's packages.
- **Resources:**
  - ClickHouse: 3 GB container, server cap 2.2e9 bytes.
  - CPU: a 3-CPU quota on shared cores (CPUQuota=300%, CPUWeight=1000, shared physical cores 4-7), never called
    "dedicated".
  - Rootless docker; every container in `measure.slice`.
- **Workload:** each 10-minute cycle runs steady inserts, updates and deletes plus one 20,000-row transaction. Then
  Postgres and ClickHouse are compared (row counts and column sums), with up to 5 minutes for CDC to catch up.
  Duration: 6 h.

## Faults (rotation, one every 40 min after the previous one ENDED)
1. **Restart ClickHouse** (`restart ch1`).
2. **Restart PeerDB's flow worker** (`restart flow-worker`).
3. **ClickHouse + Keeper outage, 10 minutes** (`outage ch1`). The Keeper is embedded, so stopping it stops the whole
   ClickHouse server.
4. **Restart Postgres, the source** (`restart source`).

**Expected:** 8 faults due before the last check: each of the 4 types twice. The exact count is what
`fault_audit.py` computes as "scheduled".

## Definitions
- **Scheduled:** a fault the injector's loop is due to run before the run's last check. The next one is due 40 min
  after the previous fault ended; an outage ends at its `start` line.
- **Applied:** a `chaos.log` action that is also visible in the target container's own log:
  - a restart: a fresh boot line within 180 s after it ("Starting ClickHouse", "Started Worker", or Postgres
    "ready to accept connections");
  - a stop: a termination/shutdown line within 60 s.
  `chaos.log` alone is not proof, because the injector logs before it acts.
- **Skipped:** any SKIP, "skip fault" or FATAL line.
- **SKIP IS FATAL in this run** (`SOAK_SKIP_FATAL=1`): an unapplied fault writes INVALID, and the stack is stopped at
  once. An INVALID run is not a pass.
- **Fault-affected cycle:** a cycle whose interval (previous check, this check] overlaps a fault window. A restart's
  window runs from its log time to +2 min; an outage's from its stop to its start +2 min. Every other cycle is
  **clean**.

## Pass criteria (ALL must hold), computed by `single_verdict.py`
1. **Valid:** no INVALID marker.
2. **Faults:** applied = scheduled; every logged action verified in container logs; 0 skipped.
3. **Duration:** at least 5.5 h of checks.
4. **Integrity:**
   - every cycle with a mismatch is followed by a cycle in which every check matches exactly;
   - the final cycle is exact.
   Together: 0 lost or corrupted rows.
5. **Freshness:**
   - 0 misses in clean cycles;
   - in fault-affected cycles, a miss is allowed, but the next cycle must be exact (rule 4);
   - the clean-cycle p99 catch-up wait is at most 150 s.
6. **Memory:** 0 query errors (Code 241).

## Validity windows
The whole run counts unless the host's `measure.log` shows the quota OFF during it (the run would be stopped and
marked invalid), or the maintainers flag host contention. A flagged window keeps integrity results. Any freshness
miss inside it is reported separately as host contention, not hidden, and not credited as a pass.

**Host conditions** are reported beside the result, from `measure.log` deltas from the start tick: load, agent CPU
pressure, and measure.slice throttled seconds.

## If it passes / fails
- **Pass:**
  - single-server v1.0.0 release notes: known limits, the upgrade path from the preview, and what is NOT in 1.0
    (replicated layouts);
  - the v1.0.0 patch goes through patch → PR → merge, and a maintainer tags it.
- **Fail:** no tag. The failing criteria and their raw tallies get reported.
