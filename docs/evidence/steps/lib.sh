# Each step prints the command, then runs it for real. Nothing on screen is typed or edited by hand.
run() { printf '\033[1;32m$\033[0m \033[1m%s\033[0m\n' "$*"; sleep 1; eval "$@"; }
hold() { sleep 3; }
