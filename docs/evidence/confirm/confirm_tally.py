#!/usr/bin/env python3
"""Tally the chlift confirm run (one layout) and the host conditions beside it.
Usage: confirm_tally.py RESULTS_DIR [MEASURE_LOG]
- RESULTS_DIR: checks.ndjson + chaos.log written by the soak runner/chaos.sh.
- MEASURE_LOG: herdrmove's per-minute log (CEST timestamps): load, agent CPU pressure, measure.slice throttling.
Integrity rule (same as the main soak): every cycle with a mismatch must be followed by a cycle in which every check
matches exactly. Freshness is reported, not graded."""
import collections, json, os, re, statistics, sys
from datetime import datetime, timedelta, timezone

res = sys.argv[1]
mlog = sys.argv[2] if len(sys.argv) > 2 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "measure.log")
rows = [json.loads(l) for l in open(os.path.join(res, "checks.ndjson")) if l.strip()]
if not rows:
    sys.exit("no checks yet")
t = lambda s: datetime.strptime(s[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
first, last = t(rows[0]["ts"]), t(rows[-1]["ts"])
by = collections.defaultdict(list)
for r in rows:
    by[r["cycle"]].append(r)
miss = sorted({r["cycle"] for r in rows if not r["match"]})
recover = {c: (all(x["match"] for x in by[c + 1]) if by.get(c + 1) else None) for c in miss}
lag = sum(1 for r in rows if not r["match"] and r["ch"])
qerr = sum(1 for r in rows if not r["match"] and not r["ch"])
w = sorted(r["waited_s"] for r in rows if r["match"])
layouts = sorted({r["variant"] for r in rows})
faults = collections.Counter()
cp = os.path.join(res, "chaos.log")
if os.path.exists(cp):
    for l in open(cp):
        a = " ".join(l.split()[1:3])
        if not a.startswith(("skip", "start keeper", "chaos")):
            faults[a] += 1

print(f"chlift confirm run: layouts {layouts}, {len(by)} cycles, {len(rows)} checks, {first:%H:%M}Z to {last:%H:%M}Z "
      f"({(last - first).total_seconds() / 3600:.1f} h)")
print("CPU: quota-based: CPUQuota=200%, CPUWeight=1000, shared physical cores 4-7 (not pinned cores)")
print(f"exact {len(rows) - lag - qerr}/{len(rows)}, lag misses {lag}, query errors {qerr}")
print(f"matched waits: p50 {w[len(w) // 2]} s, p99 {w[int(.99 * (len(w) - 1))]} s, max {w[-1]} s" if w else "no matched checks")
print("miss cycles and recovery: " + (", ".join(f"{c}->{'pending' if ok is None else 'yes' if ok else 'NO'}" for c, ok in recover.items()) or "none"))
ev = [r for r in rows if r["table"] == "events"]
rate = (int(ev[-1]["pg"].split("|")[0]) - int(ev[0]["pg"].split("|")[0])) / max((t(ev[-1]["ts"]) - t(ev[0]["ts"])).total_seconds(), 1)
print(f"source events insert rate: {rate:.0f} rows/s average")
print("faults: " + (", ".join(f"{k} x{n}" for k, n in sorted(faults.items())) or "none") + f" (total {sum(faults.values())})")
v = list(recover.values())
print("INTEGRITY: " + ("FAIL: a miss did not recover" if False in v else "PENDING: a miss cycle has no next cycle yet" if None in v
                       else "PASS: no lost or corrupted rows; every miss matched exactly at the next comparison"))

# Host conditions from measure.log, inside the run window. CEST = UTC+2 on these dates.
ticks = []
if os.path.exists(mlog):
    for l in open(mlog):
        m = re.match(r"(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) CEST tick load=([\d.]+) .*?measure_procs=(\d+) agent_cpu_psi_avg10=([\d.]+) "
                     r"nr_throttled=(\d+) throttled_usec=(\d+)", l)
        if m:
            ts = datetime.strptime(m[1], "%Y-%m-%d %H:%M:%S").replace(tzinfo=timezone.utc) - timedelta(hours=2)
            if first - timedelta(minutes=15) <= ts <= last:
                ticks.append((ts, float(m[2]), int(m[3]), float(m[4]), int(m[5]), int(m[6])))
if ticks:
    loads = sorted(x[1] for x in ticks); psi = sorted(x[3] for x in ticks)
    thr = ticks[-1][5] - ticks[0][5]
    span = (ticks[-1][0] - ticks[0][0]).total_seconds()
    print(f"host ({len(ticks)} measure.log ticks): load1 p50 {loads[len(loads) // 2]:.1f}, max {loads[-1]:.1f}; agent CPU pressure "
          f"avg10 p50 {psi[len(psi) // 2]:.0f}%, max {psi[-1]:.0f}%; measure.slice throttled {thr / 1e6:.0f} s over "
          f"{span / 3600:.1f} h ({100 * thr / 1e6 / span if span else 0:.1f}% of wall time)")
else:
    print(f"host: no measure.log ticks inside the run window ({mlog})")
