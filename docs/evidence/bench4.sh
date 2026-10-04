#!/usr/bin/env bash
# One invocation: Postgres, ClickHouse (FINAL and plain), DuckDB reading Postgres, DuckDB on a local Parquet copy.
# Same 4 queries as numbers.md rows 9/11, 5 runs each. Writes bench4-raw.json; recompute with bench4_table.py.
# Env: CHLIFT (chlift binary), CHLIFT_PG_DSN + chlift secrets (for chlift migrate bench), PG_KV (libpq key/value
# string for DuckDB's ATTACH), PLAN (chlift migration plan), WORK (scratch dir for the Parquet copy).
set -euo pipefail
OUT=${OUT:-bench4-raw.json} RUNS=5
WORK=${WORK:?scratch dir} ; mkdir -p "$WORK"
T=$(mktemp -d)
load() { printf '{"utc":"%s","loadavg":"%s","cpu_pressure_some_avg60":%s}' "$(date -u +%FT%TZ)" \
  "$(cut -d' ' -f1-3 /proc/loadavg)" "$(sed -n 's/^some .*avg60=\([0-9.]*\).*/\1/p' /proc/pressure/cpu)"; }

# The 4 queries, Postgres dialect, exactly as chlift migrate bench sends them to Postgres.
Q=("SELECT date_trunc('day', event_time) d, count(DISTINCT user_id) FROM user_events WHERE event_time > now() - interval '30 days' GROUP BY 1 ORDER BY 1"
   "SELECT date_trunc('week', event_time) w, kind, count(*) FROM user_events GROUP BY 1, 2 ORDER BY 1, 2"
   "SELECT account_id, count(*) c FROM user_events WHERE event_time > now() - interval '7 days' GROUP BY 1 ORDER BY c DESC LIMIT 10"
   "SELECT path, percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) FROM request_logs WHERE created_at > now() - interval '30 days' GROUP BY 1 ORDER BY 1")

L0=$(load)
# 1) Postgres + ClickHouse, chlift's own bench (raw samples into pgch.json)
"$CHLIFT" migrate bench --plan "$PLAN" --runs $RUNS --out "$T/pgch.json" > "$T/pgch.txt"
L1=$(load)

# DuckDB session: every query RUNS times, one line "Run Time (s): real X" per run; then the row count of each result.
duck() { # $1 = setup SQL, $2 = timing file, $3 = row-count file
  { echo "$1"; echo ".timer on"; echo ".output /dev/null"
    for q in "${Q[@]}"; do for _ in $(seq $RUNS); do echo "$q;"; done; done
    echo ".timer off"; echo ".output $3"; echo ".mode list"; echo ".headers off"
    for q in "${Q[@]}"; do echo "SELECT count(*) FROM ($q);"; done
  } | duckdb 2> "$2.err" | grep '^Run Time' > "$2"
}
# 2) DuckDB (a): reads Postgres directly through the postgres extension
SETUP_A="LOAD postgres; SET memory_limit='2GB'; SET temp_directory='$WORK/spill'; SET TimeZone='UTC'; ATTACH '$PG_KV' AS pg (TYPE postgres, READ_ONLY); USE pg.public;"
duck "$SETUP_A" "$T/duck_pg.t" "$T/duck_pg.rows"
L2=$(load)

# 3) Copy the two tables to local Parquet (timed), then DuckDB (b) on the copy
s=$(date +%s.%N)
duckdb -c "LOAD postgres; SET memory_limit='2GB'; SET temp_directory='$WORK/spill'; SET TimeZone='UTC'; ATTACH '$PG_KV' AS pg (TYPE postgres, READ_ONLY);
  COPY (SELECT * FROM pg.public.user_events) TO '$WORK/user_events.parquet' (FORMAT parquet);
  COPY (SELECT * FROM pg.public.request_logs) TO '$WORK/request_logs.parquet' (FORMAT parquet);" > /dev/null
copy_s=$(echo "$(date +%s.%N) - $s" | bc)
size=$(du -cb "$WORK"/user_events.parquet "$WORK"/request_logs.parquet | tail -1 | cut -f1)
L3=$(load)
SETUP_B="SET memory_limit='2GB'; SET temp_directory='$WORK/spill'; SET TimeZone='UTC'; CREATE VIEW user_events AS SELECT * FROM read_parquet('$WORK/user_events.parquet');
  CREATE VIEW request_logs AS SELECT * FROM read_parquet('$WORK/request_logs.parquet');"
duck "$SETUP_B" "$T/duck_pq.t" "$T/duck_pq.rows"
L4=$(load)

# Postgres row counts of each result, to prove the engines answered the same question
for q in "${Q[@]}"; do psql "$CHLIFT_PG_DSN" -Atc "SELECT count(*) FROM ($q) x"; done > "$T/pg.rows"

python3 - "$T" "$OUT" "$copy_s" "$size" "$RUNS" "$L0" "$L1" "$L2" "$L3" "$L4" "$(duckdb --version)" "$(nproc)" <<'PY'
import json, sys
T, out, copy_s, size, runs = sys.argv[1], sys.argv[2], float(sys.argv[3]), int(sys.argv[4]), int(sys.argv[5])
loads = [json.loads(x) for x in sys.argv[6:11]]
pgch = json.load(open(f"{T}/pgch.json"))
def times(f):
    ms = [round(float(l.split()[4]) * 1000, 1) for l in open(f)]
    assert len(ms) == 4 * runs, f"{f}: {len(ms)} timings, expected {4 * runs} (a query failed? see {f}.err)"
    return [ms[i * runs:(i + 1) * runs] for i in range(4)]
rows = {k: [int(x) for x in open(f"{T}/{k}.rows").read().split()] for k in ("pg", "duck_pg", "duck_pq")}
a, b = times(f"{T}/duck_pg.t"), times(f"{T}/duck_pq.t")
for i, r in enumerate(pgch["results"]):
    r["duckdb_postgres_runs_ms"], r["duckdb_parquet_runs_ms"] = a[i], b[i]
    r["result_rows"] = {k: v[i] for k, v in rows.items()}
pgch.update({"duckdb_version": sys.argv[11], "duckdb_threads_cpus": int(sys.argv[12]), "duckdb_memory_limit": "2GB (agent user memory is capped on this shared box)",
             "parquet_copy_seconds": round(copy_s, 1), "parquet_bytes": size,
             "load": dict(zip(["start", "after_pg_clickhouse", "after_duckdb_postgres", "after_copy", "after_duckdb_parquet"], loads))})
json.dump(pgch, open(out, "w"), indent=1)
print("wrote", out)
PY
