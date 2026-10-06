#!/usr/bin/env python3
"""Verdict for the single-server fault soak, by the criteria in SINGLE-SOAK-CRITERIA.md (fixed before the run).
Usage: single_verdict.py RESULTS_DIR PROJECT [MEASURE_LOG]
A cycle's interval is (previous check, this check]. It is FAULT-AFFECTED if any fault overlaps it: a restart counts
from its log time to +2 min; an outage from its stop to its start +2 min. Every other cycle is CLEAN."""
import collections, json, os, re, statistics, subprocess, sys
from datetime import datetime, timedelta, timezone

res, project = sys.argv[1], sys.argv[2]
mlog = sys.argv[3] if len(sys.argv) > 3 else os.environ.get("MEASURE_LOG", os.path.join(os.path.dirname(os.path.abspath(__file__)), "measure.log"))
here = os.path.dirname(os.path.abspath(__file__))
UTC = timezone.utc
pt = lambda s: datetime.strptime(s[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=UTC)
rows = [json.loads(l) for l in open(f"{res}/checks.ndjson") if l.strip()]
by = collections.OrderedDict()
for r in rows:
    by.setdefault(r["cycle"], []).append(r)

# fault windows
acts = []
for l in open(f"{res}/chaos.log"):
    m = re.match(r"(\S+) (restart|stop|start) ([a-z0-9-]+) \(", l)
    if m:
        acts.append((pt(m[1]), m[2], m[3]))
win = []
for i, (t, a, s) in enumerate(acts):
    if a == "restart":
        win.append((t, t + timedelta(minutes=2), f"restart {s}"))
    elif a == "stop":
        e = next((u for u, b, x in acts[i + 1:] if b == "start" and x == s), t + timedelta(minutes=10))
        win.append((t, e + timedelta(minutes=2), f"outage {s}"))

prev, clean, affected = None, [], []
for c, xs in by.items():
    end = max(pt(x["ts"]) for x in xs)
    beg = prev if prev else end - timedelta(minutes=15)
    hit = [w[2] for w in win if w[0] < end and w[1] > beg]
    (affected if hit else clean).append((c, xs, hit))
    prev = end

first, last = pt(rows[0]["ts"]), pt(rows[-1]["ts"])
hours = (last - first).total_seconds() / 3600
lag = [r for r in rows if not r["match"] and r["ch"]]
qerr = [r for r in rows if not r["match"] and not r["ch"]]
# Criterion 6 is about MEMORY (Code 241). Other query errors (e.g. ClickHouse unreachable during an outage) are graded
# like misses under criterion 5: allowed in fault-affected cycles if the next cycle is exact, failing in clean cycles.
mem = [r for r in qerr if re.search(r"241|MEMORY_LIMIT", r.get("err", ""))]
cyc = list(by)
miss_cycles = sorted({r["cycle"] for r in rows if not r["match"]})
recover = {c: (all(x["match"] for x in by[c + 1]) if c + 1 in by else None) for c in miss_cycles}
clean_miss = [c for c, xs, _ in clean if any(not x["match"] for x in xs)]
aff_miss = [(c, h) for c, xs, h in affected if any(not x["match"] for x in xs)]
w_clean = sorted(x["waited_s"] for c, xs, _ in clean for x in xs if x["match"])
p99 = w_clean[int(.99 * (len(w_clean) - 1))] if w_clean else None
final_exact = all(x["match"] for x in by[cyc[-1]])
invalid = os.path.exists(f"{res}/INVALID")
audit = subprocess.run([sys.executable, os.path.join(here, "fault_audit.py"), res, project], capture_output=True, text=True)

print(f"chlift single-server fault soak: {len(by)} cycles, {len(rows)} checks, {first:%H:%M}Z to {last:%H:%M}Z ({hours:.1f} h)")
print("CPU: 3-CPU quota on shared cores (CPUQuota=300%, CPUWeight=1000, shared physical cores 4-7; not dedicated cores)")
print(f"exact {len(rows) - len(lag) - len(qerr)}/{len(rows)}, lag misses {len(lag)}, query errors {len(qerr)} (Code 241: {len(mem)})")
for r in qerr:
    print(f"  query error cycle {r['cycle']} {r['ts']} {r['table']}: {r.get('err', '')[:120]}")
print(f"cycles: {len(clean)} clean, {len(affected)} fault-affected")
print(f"clean-cycle catch-up waits: p50 {w_clean[len(w_clean) // 2] if w_clean else '-'} s, p99 {p99} s, max {w_clean[-1] if w_clean else '-'} s")
print("misses in fault-affected cycles: " + (", ".join(f"{c} ({'; '.join(h)}) -> next exact: {recover.get(c)}" for c, h in aff_miss) or "none"))
print("misses in clean cycles: " + (", ".join(map(str, clean_miss)) or "none"))
print(audit.stdout.strip())
# Host conditions beside the result (criteria: from measure.log, deltas from the start tick). measure.log is CEST (UTC+2).
ticks = []
if os.path.exists(mlog):
    for l in open(mlog):
        m = re.match(r"(\S+ \S+) CEST tick load=([\d.]+) .*?agent_cpu_psi_avg10=([\d.]+) nr_throttled=(\d+) throttled_usec=(\d+)", l)
        if m:
            ts = datetime.strptime(m[1], "%Y-%m-%d %H:%M:%S").replace(tzinfo=UTC) - timedelta(hours=2)
            if first - timedelta(minutes=15) <= ts <= last:
                ticks.append((ts, float(m[2]), float(m[3]), int(m[5])))
if ticks:
    ld = sorted(t[1] for t in ticks); ps = sorted(t[2] for t in ticks)
    thr = (ticks[-1][3] - ticks[0][3]) / 1e6; span = (ticks[-1][0] - ticks[0][0]).total_seconds()
    print(f"host ({len(ticks)} measure.log ticks, deltas from {ticks[0][0]:%H:%M}Z): load1 p50 {ld[len(ld) // 2]:.1f}, max {ld[-1]:.1f}; "
          f"agent CPU pressure avg10 p50 {ps[len(ps) // 2]:.0f}%, max {ps[-1]:.0f}%; measure.slice throttled {thr:.0f} s over "
          f"{span / 3600:.1f} h ({100 * thr / span if span else 0:.1f}% of wall time)")
else:
    print(f"host: NO measure.log ticks in the run window ({mlog})")

crit = [
    ("run valid: no INVALID marker (no fatal skip)", not invalid),
    ("fault audit: every scheduled fault applied and verified in container logs, 0 skipped", audit.returncode == 0),
    (f"duration >= 5.5 h (got {hours:.1f} h)", hours >= 5.5),
    ("integrity: every cycle with a mismatch is followed by an exact cycle", None not in recover.values() and all(recover.values())),
    ("integrity: the final cycle is exact", final_exact),
    (f"freshness: 0 misses in clean cycles (got {len(clean_miss)})", not clean_miss),
    (f"freshness: clean-cycle p99 catch-up <= 150 s (got {p99} s)", p99 is not None and p99 <= 150),
    (f"memory: 0 Code 241 errors (got {len(mem)}; {len(qerr) - len(mem)} other query errors graded as misses under 5)", not mem),
]
for name, ok in crit:
    print(("PASS " if ok else "FAIL ") + name)
print("CALL: " + ("PASS: the single-server v1.0.0 gate is met" if all(ok for _, ok in crit) else "FAIL: the v1.0.0 gate is NOT met"))
