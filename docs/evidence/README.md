# Evidence

- `numbers.md`: every figure we publish, with its source file and its caveat.
- `casts/`: asciinema v2 terminal recordings with real timing (play with `asciinema play casts/03-migrate-start.cast`).
  Each `.log` is the same output as plain text. `recording-times.log` lists when each step ran (UTC).
- `steps/`: the exact scripts that ran. Each prints a command and then runs it; the output is unedited. `record.sh`
  records one step, and `all.sh` ran them in order.
- `bench-raw.json` + `bench_table.py`: every benchmark sample, and the script that recomputes the published table
  from them.
- `claims.tsv` + `verify_numbers.py`: every figure in `numbers.md` is matched to the exact log line it comes from (a pair
  must come from one line), and the latency table is recomputed from `bench-raw.json`. CI fails on any mismatch, and on
  any large number that no claim covers.
