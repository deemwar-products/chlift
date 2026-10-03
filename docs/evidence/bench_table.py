#!/usr/bin/env python3
"""Recompute the benchmark table from a `chlift migrate bench` raw file: every number shown is the median
of the samples stored in that file. Usage: bench_table.py bench-raw.json"""
import json, statistics, sys

d = json.load(open(sys.argv[1]))
print(f"runs per query: {d['runs']}, taken {d['taken_utc']}")
print(f"{'query':42} {'postgres':>10} {'ch FINAL':>10} {'ch':>8}  speed-up FINAL / after cutover")
for r in d["results"]:
    pg, fin, ch = (statistics.median(r[k]) for k in ("postgres_runs_ms", "clickhouse_final_runs_ms", "clickhouse_runs_ms"))
    assert (pg, fin, ch) == (r["postgres_ms"], r["clickhouse_final_ms"], r["clickhouse_ms"]), "stored medians do not match the samples"
    print(f"{r['query']:42} {pg:>8.0f}ms {fin:>8.0f}ms {ch:>6.0f}ms  {pg/fin:.0f}x / {pg/ch:.0f}x")
