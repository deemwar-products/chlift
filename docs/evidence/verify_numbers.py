#!/usr/bin/env python3
"""Fail if numbers.md quotes a figure the evidence does not show.

1. Every claim in claims.tsv must appear verbatim in numbers.md, and its regex(es) must match a line of its log.
2. The latency table and the speed-up ranges in numbers.md must equal what bench-raw.json recomputes to.
3. Every large number (4+ digits) in numbers.md must be covered by a claim or by the bench table.
"""
import json, os, re, statistics, sys

here = os.path.dirname(os.path.abspath(__file__))
doc = open(os.path.join(here, "numbers.md")).read()
fail = []
covered = set()

def nums(s):
    return {n.replace(",", "") for n in re.findall(r"\d{1,3}(?:,\d{3})+|\d{4,}", s)}

for line in open(os.path.join(here, "claims.tsv")):
    if not line.strip() or line.startswith("#"):
        continue
    text, log, patterns = line.rstrip("\n").split("\t")
    if text not in doc:
        fail.append(f"claim not found verbatim in numbers.md: {text!r}")
    lines = open(os.path.join(here, log)).read().splitlines()
    for pat in patterns.split(" && "):
        if not any(re.search(pat, l) for l in lines):
            fail.append(f"{log}: no line matches {pat!r} (claim {text!r})")
    covered |= nums(text)

raw = json.load(open(os.path.join(here, "bench-raw.json")))
fin, plain = [], []
for r in raw["results"]:
    pg, f, c = (statistics.median(r[k]) for k in ("postgres_runs_ms", "clickhouse_final_runs_ms", "clickhouse_runs_ms"))
    cells = f" | {pg:,.0f} | {f:,.0f} | {c:,.0f} |"
    names = (r["query"], r["query"][0].upper() + r["query"][1:])
    row = next((f"| {n}{cells}" for n in names if f"| {n}{cells}" in doc), f"| {names[1]}{cells}")
    if row not in doc:
        fail.append(f"bench row missing or different in numbers.md: {row}")
    covered |= nums(row)
    fin.append(pg / f); plain.append(pg / c)
for label, xs in (("FINAL", fin), ("after cutover", plain)):
    rng = f"**{min(xs):.0f}× to {max(xs):.0f}×**"
    if rng not in doc:
        fail.append(f"speed-up range for {label} should read {rng}")

# The 2026-10-04 rerun with DuckDB (bench4-raw.json): every table row and range recomputed the same way.
b4 = json.load(open(os.path.join(here, "bench4-raw.json")))
b4cols = ("postgres_runs_ms", "clickhouse_final_runs_ms", "clickhouse_runs_ms", "duckdb_postgres_runs_ms", "duckdb_parquet_runs_ms")
b4x = {k: [] for k in b4cols[1:]}
for r in b4["results"]:
    m = [statistics.median(r[k]) for k in b4cols]
    cells = " | ".join(f"{x:,.0f}" for x in m) + " |"
    names = (r["query"], r["query"][0].upper() + r["query"][1:])
    row = next((f"| {n} | {cells}" for n in names if f"| {n} | {cells}" in doc), f"| {names[1]} | {cells}")
    if row not in doc:
        fail.append(f"bench4 row missing or different in numbers.md: {row}")
    covered |= nums(row)
    if len(set(r["result_rows"].values())) != 1:
        fail.append(f"bench4: engines returned different row counts for {r['query']}: {r['result_rows']}")
    for k in b4cols[1:]:
        b4x[k].append(m[0] / statistics.median(r[k]))
for k, xs in b4x.items():
    rng = f"{min(xs):.1f}× to {max(xs):.1f}×"
    if rng not in doc:
        fail.append(f"bench4 range for {k} should read {rng}")

ignore = {"2026", "4746"}  # a year, a PeerDB issue number
for n in sorted(nums(re.sub(r"\d{4}-\d{2}-\d{2}", "", doc)) - covered - ignore):
    fail.append(f"figure {n} in numbers.md is not backed by claims.tsv or bench-raw.json")

for f in fail:
    print("FAIL", f)
print(f"{'ok' if not fail else 'FAILED'}: {len(fail)} problem(s)")
sys.exit(1 if fail else 0)
