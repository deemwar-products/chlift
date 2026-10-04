#!/usr/bin/env python3
"""Recompute soak verdict numbers from the read-only mirror. Rule (CEO 2026-10-04, written before cycle 82 was seen):
valid = ts < 06:00Z, or ts >= the 06:41:07Z resume. Freshness miss = match false with a ch value; query error = empty ch.
CEO d8 2026-10-04: 10:09-11:20Z is "host contention on the soak cores" (other agents on cpus 6,7): integrity counts,
freshness misses there are reported separately as contention, never as a chlift sizing finding."""
import json, collections, os, sys
P = next((a for a in sys.argv[1:] if not a.startswith('--')), None) or os.path.join(os.path.dirname(os.path.abspath(__file__)), 'checks.ndjson')
rows = [json.loads(l) for l in open(P) if l.strip()]
valid = lambda t: t < '2026-10-04T06:00' or t >= '2026-10-04T06:41:07'
c = collections.defaultdict(lambda: collections.Counter())
for r in rows:
    k = 'excluded' if not valid(r['ts']) else 'contention' if '2026-10-04T10:09' <= r['ts'] < '2026-10-04T11:20' else 'valid'
    v = c[(k, r['variant'])]; v['checks'] += 1
    if not r['match']:
        v['query_error' if not r['ch'] else 'lag_miss'] += 1
        v['miss_cycles_' + str(r['cycle'])] += 1
for key in sorted(c): print(*key, dict(c[key]))
cyc = collections.OrderedDict()
for r in rows:
    if '2026-10-04T10:09' <= r['ts'] < '2026-10-04T11:20': cyc.setdefault(r['cycle'], []).append(r)
for n, x in cyc.items():
    print('contention cycle', n, x[0]['ts'], len(x), 'checks', sum(not r['match'] for r in x), 'misses, max wait',
          max(r['waited_s'] for r in x), 's')
print('last ts', rows[-1]['ts'], 'last cycle', rows[-1]['cycle'])

if '--verdict' in sys.argv:
    from datetime import datetime as D
    t = lambda s: D.strptime(s[:19], '%Y-%m-%dT%H:%M:%S')
    last = rows[-1]['ts']
    hours = ((t('2026-10-04T06:00:00') - t('2026-10-03T16:02:00')) + (t(last) - t('2026-10-04T06:41:07'))).total_seconds() / 3600
    cont = (min(t(last), t('2026-10-04T11:20:00')) - t('2026-10-04T10:09:00')).total_seconds() / 3600
    # integrity: every cycle with a lag miss must be followed by a cycle in which every check matched exactly
    bycyc = collections.defaultdict(list)
    for r in rows: bycyc[r['cycle']].append(r)
    misscyc = sorted({r['cycle'] for r in rows if not r['match'] and valid(r['ts'])})
    # None = the next cycle has not been checked yet (pending, not a failure)
    recover = {c: (all(x['match'] for x in bycyc[c + 1]) if bycyc.get(c + 1) else None) for c in misscyc}
    faults = collections.Counter()
    for l in open(P.replace('checks.ndjson', 'chaos.log')):
        ts, act = l.split()[0], ' '.join(l.split()[1:3])
        if valid(ts) and not act.startswith(('skip', 'start keeper', 'chaos')): faults[act] += 1
    print(f'\n## Verdict numbers (soak_tally.py --verdict, last check {last}, cycle {rows[-1]["cycle"]})\n')
    print(f'Valid soak time: {hours:.1f} h (16:02Z-06:00Z + 06:41:07Z-{last[11:19]}Z), of which {cont:.1f} h host contention (10:09-11:20Z).\n')
    print('| Layout | Valid checks | Exact | Lag misses | Query errors (Code 241) |\n|---|---|---|---|---|')
    for v in ('safe', 'clusq', 'clusnq'):
        a = c[('valid', v)] + c[('contention', v)]
        print(f"| `{v}` | {a['checks']} | {a['checks'] - a['lag_miss'] - a['query_error']} | {a['lag_miss']} | {a['query_error']} |")
    print('\nMiss cycles and recovery (next cycle all exact?): ' + ', '.join(f'{k}->{"pending" if ok is None else "yes" if ok else "NO"}' for k, ok in recover.items()))
    print('Faults survived in valid time: ' + ', '.join(f'{k} x{n}' for k, n in sorted(faults.items())) + f' (total {sum(faults.values())})')
    vals = list(recover.values())
    print('INTEGRITY: ' + ('FAIL: a miss did not recover' if False in vals else 'PENDING: a miss cycle has no next cycle yet' if None in vals
                           else 'PASS: no lost or corrupted rows; every miss matched exactly at the next comparison'))
    # sizing figures for the requirements (whole soak stack on 2 cores; 3 mirrors from one source into the same 2 replicas)
    import re, statistics
    w = sorted(r['waited_s'] for r in rows if r['variant'] == 'safe' and valid(r['ts']) and r['match'])
    print(f'safe matched waits: p50 {w[len(w)//2]} s, p99 {w[int(.99*(len(w)-1))]} s, max {w[-1]} s')
    busy = []
    for l in open(P.replace('checks.ndjson', 'contention.log')):
        ts = l.split()[0]
        m = re.search(r'cpu6=(\d+)% cpu7=(\d+)%', l)
        if m and valid(ts) and not ('2026-10-04T10:09' <= ts < '2026-10-04T11:20'):
            busy.append((int(m[1]) + int(m[2])) / 2)
    busy.sort()
    print(f'soak cores busy (valid time, contention excluded): mean {statistics.mean(busy):.0f}%, p90 {busy[int(.9*len(busy))]:.0f}%')
    ev = [r for r in rows if r['table'] == 'events' and r['variant'] == 'safe']
    secs = (t(ev[-1]['ts']) - t(ev[0]['ts'])).total_seconds()
    print(f"source events insert rate: {(int(ev[-1]['pg'].split('|')[0]) - int(ev[0]['pg'].split('|')[0])) / secs:.0f} rows/s average")
    sc = {r['cycle'] for r in rows if r['variant'] == 'safe' and valid(r['ts'])}
    sm = sorted({r['cycle'] for r in rows if r['variant'] == 'safe' and valid(r['ts']) and not r['match']})
    print(f"safe layout: {len(sc)} cycles counted, freshness misses in {len(sm)} ({', '.join(map(str, sm))})")
