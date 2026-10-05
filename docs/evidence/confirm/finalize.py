#!/usr/bin/env python3
"""Apply the confirm-run criteria fixed in CONFIRM-VERDICT.md (2026-10-04 21:15 UTC) to confirm_tally.py's output.
Writes RESULTS/verdict.txt (the tally, verbatim) and prints one line per criterion plus the call.
Usage: finalize.py RESULTS_DIR [MEASURE_LOG]"""
import os, re, subprocess, sys

res = sys.argv[1]
here = os.path.dirname(os.path.abspath(__file__))
out = subprocess.run([sys.executable, os.path.join(here, "confirm_tally.py"), *sys.argv[1:]],
                     capture_output=True, text=True, check=True).stdout
open(os.path.join(res, "verdict.txt"), "w").write(out)
print(out)

hours = float(re.search(r"\(([\d.]+) h\)", out)[1])
lag = int(re.search(r"lag misses (\d+)", out)[1])
qerr = int(re.search(r"query errors (\d+)", out)[1])
p99 = int(re.search(r"p99 (\d+) s", out)[1])
integrity = "INTEGRITY: PASS" in out
crit = [
    ("integrity: every miss followed by an exact cycle", integrity),
    (f"duration >= 5.5 h (got {hours} h)", hours >= 5.5),
    (f"0 freshness misses (got {lag})", lag == 0),
    (f"p99 catch-up wait <= 150 s (got {p99} s)", p99 <= 150),
    (f"0 query errors / Code 241 (got {qerr})", qerr == 0),
]
for name, ok in crit:
    print(("PASS " if ok else "FAIL ") + name)
if not integrity:
    print("CALL: INTEGRITY FAIL")
elif all(ok for _, ok in crit):
    print("CALL: 2 cores per mirror CONFIRMED (quota-based: CPUQuota=200%, CPUWeight=1000, shared physical cores 4-7)")
else:
    print("CALL: NOT confirmed; raise the README CPU figure and report the misses with their host conditions")
