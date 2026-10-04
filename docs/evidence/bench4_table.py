#!/usr/bin/env python3
"""Recompute the 4-way table from bench4-raw.json: median of every run list, plus speed-ups against Postgres.
Fails if DuckDB and Postgres returned a different number of result rows for any query."""
import json, statistics, sys
d = json.load(open(sys.argv[1] if len(sys.argv) > 1 else "bench4-raw.json"))
cols = [("postgres_runs_ms", "Postgres"), ("clickhouse_final_runs_ms", "ClickHouse FINAL"), ("clickhouse_runs_ms", "ClickHouse"),
        ("duckdb_postgres_runs_ms", "DuckDB reading Postgres"), ("duckdb_parquet_runs_ms", "DuckDB on Parquet copy")]
print(f"taken {d['taken_utc']}, runs {d['runs']}, DuckDB {d['duckdb_version']} on {d['duckdb_threads_cpus']} CPUs; "
      f"Parquet copy {d['parquet_copy_seconds']} s, {d['parquet_bytes'] / 1e6:.0f} MB")
print("| Query (median ms) | " + " | ".join(c[1] for c in cols) + " |\n|" + "---|" * (len(cols) + 1))
bad = []
for r in d["results"]:
    m = [statistics.median(r[k]) for k, _ in cols]
    name = r["query"] if r["query"].startswith("p95") else r["query"][0].upper() + r["query"][1:]
    print(f"| {name} | " + " | ".join(f"{x:,.0f}" for x in m) + " |")
    rr = r["result_rows"]
    if len(set(rr.values())) != 1:
        bad.append(f"{r['query']}: result rows differ {rr}")
for k, name in cols[1:]:
    xs = [statistics.median(r["postgres_runs_ms"]) / statistics.median(r[k]) for r in d["results"]]
    print(f"{name}: {min(xs):.1f}x to {max(xs):.1f}x vs Postgres")
for name, l in d["load"].items():
    print(f"load {name}: {l['utc']} loadavg {l['loadavg']} cpu pressure avg60 {l['cpu_pressure_some_avg60']}%")
for b in bad:
    print("FAIL", b)
sys.exit(1 if bad else 0)
