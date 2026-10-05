#!/usr/bin/env python3
"""Audit every fault in chaos.log against the target container's own logs: APPLIED only if the container shows it.
Usage: fault_audit.py RESULTS_DIR PROJECT [FAULT_EVERY_S]
- scheduled = fault ticks that fall inside the run window ('chaos start, until epoch E' line; one tick every FAULT_EVERY_S)
- applied   = a chaos.log action (restart/stop/start) whose container log shows the matching event in time
- skipped   = SKIP / 'skip fault' / FATAL lines
Exit 1 unless every scheduled fault is applied, nothing is skipped, and every logged action is verified."""
import json, re, subprocess, sys
from datetime import datetime, timedelta, timezone

res, project = sys.argv[1], sys.argv[2]
every = int(sys.argv[3]) if len(sys.argv) > 3 else 2400
UTC = timezone.utc
lines = open(f"{res}/chaos.log").read().splitlines()
pt = lambda s: datetime.strptime(s[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=UTC)

start = end = None
acts, skips = [], []
for l in lines:
    ts, rest = l.split(" ", 1)
    m = re.match(r"chaos start, until epoch (\d+)", rest)
    if m:
        start, end = pt(ts), datetime.fromtimestamp(int(m[1]), UTC)
    elif re.match(r"(restart|stop|start) ([a-z0-9-]+) \(", rest):
        a, svc = rest.split()[:2]
        acts.append((pt(ts), a, svc))
    elif re.search(r"SKIP|skip fault|FATAL", rest):
        skips.append(l)
# Scheduled = faults the injector would have run before the run's last check. The loop sleeps FAULT_EVERY_S after the
# previous fault ENDED (an outage's end is its 'start' line), so due times drift with outages.
checks = [json.loads(l) for l in open(f"{res}/checks.ndjson") if l.strip()]
last_check = pt(checks[-1]["ts"])
faults_logged = [(t, a, s) for t, a, s in acts if a in ("restart", "stop")]
ends = {}
for i, (t, a, s) in enumerate(acts):
    if a == "stop":
        nxt = next((u for u, b, x in acts[i + 1:] if b == "start" and x == s), t)
        ends[t] = nxt
scheduled, missing, due = 0, [], start + timedelta(seconds=every)
for t, a, s in faults_logged:
    while due <= last_check and t - due > timedelta(seconds=120):   # a due tick with no fault near it
        scheduled += 1; missing.append(due); due += timedelta(seconds=every)
    if due > last_check:
        break
    scheduled += 1
    due = ends.get(t, t) + timedelta(seconds=every)
while due <= last_check:
    scheduled += 1; missing.append(due); due += timedelta(seconds=every)

def container_log(svc):
    name = f"{project}-{svc}-1"
    txt = subprocess.run(["docker", "logs", "-t", name], capture_output=True, text=True).stdout
    txt += subprocess.run(["docker", "logs", "-t", name], capture_output=True, text=True).stderr
    for f in ("/var/log/clickhouse-server/clickhouse-server.log", "/var/log/clickhouse-keeper/clickhouse-keeper.log"):
        cp = subprocess.run(["docker", "cp", f"{name}:{f}", "-"], capture_output=True)
        if cp.returncode == 0:
            tar = subprocess.run(["tar", "-xO"], input=cp.stdout, capture_output=True)
            txt += tar.stdout.decode("utf-8", "replace")
    return txt

STOP = r"Received termination signal|database system is shut down|received fast shutdown request|received smart shutdown request"
UP = r"Starting ClickHouse|\"Started Worker\"|database system is ready to accept connections"
TSRE = re.compile(r"(\d{4}[-.]\d\d[-.]\d\d[T ]\d\d:\d\d:\d\d)")
logs, events = {}, {}
for _, _, svc in acts:
    if svc in events:
        continue
    ev = []
    for l in container_log(svc).splitlines():
        m = TSRE.search(l)
        if not m:
            continue
        t = datetime.strptime(m[1].replace(".", "-").replace("T", " "), "%Y-%m-%d %H:%M:%S").replace(tzinfo=UTC)
        kind = "stop" if re.search(STOP, l) else "up" if re.search(UP, l) else None
        if kind:
            ev.append((t, kind))
    events[svc] = ev

def seen(svc, kind, t0, t1):
    return any(t0 <= t <= t1 and k == kind for t, k in events[svc])

ok = []
for t, a, svc in acts:
    if a == "restart":
        # a fresh boot line after the restart command proves it (processes log startup only when they boot);
        # some (PeerDB's Go worker) never log a shutdown line, so the stop line is not required here
        v = seen(svc, "up", t, t + timedelta(seconds=180))
    elif a == "stop":
        v = seen(svc, "stop", t - timedelta(seconds=5), t + timedelta(seconds=60))
    else:  # start
        v = seen(svc, "up", t - timedelta(seconds=5), t + timedelta(seconds=180))
    ok.append(v)
    print(f"{'VERIFIED  ' if v else 'UNVERIFIED'} {t:%H:%M:%S}Z {a} {svc}")
faults = sum(1 for t, a, _ in acts if a in ("restart", "stop") and t <= last_check)
print(f"scheduled {scheduled} (due before the last check {last_check:%H:%M:%S}Z), applied {faults} (actions logged {len(acts)}, verified {sum(ok)}), skipped {len(skips)}")
for d in missing:
    print(f"MISSING: a fault was due at {d:%H:%M:%S}Z and none was logged")
for s in skips:
    print("SKIP:", s)
good = faults == scheduled and not missing and not skips and all(ok)
print("FAULT AUDIT: " + ("PASS" if good else "FAIL"))
sys.exit(0 if good else 1)
