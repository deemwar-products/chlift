. "$(dirname "$0")/lib.sh"; clear
run 'chlift migrate bench --runs 5 --out bench-raw.json'
run 'python3 bench_table.py bench-raw.json'
hold
