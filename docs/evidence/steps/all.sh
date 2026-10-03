#!/bin/sh
export PATH=/tmp/chlift-demo/bin:$HOME/.venvs/asciinema/bin:$PATH TERM=xterm-256color CHLIFT_PG_DSN=postgres://postgres:postgres@127.0.0.1:55441/app
sleep 20
for s in 00-seed 01-install 02-migrate-plan 03-migrate-start 04-cdc 05-migrate-check 07-corruption-repro 06-bench; do
  echo "$(date -u +%T) start $s" >> /tmp/chlift-demo/casts/progress.log
  sh $HOME/chlift/docs/evidence/steps/record.sh $s >> /tmp/chlift-demo/casts/recorder.log 2>&1
  asciinema cat /tmp/chlift-demo/casts/$s.cast | sed "s/\x1b\[[0-9;]*[A-Za-z]//g; s/\x1b[()][AB012]//g; s/\r//g" > /tmp/chlift-demo/casts/$s.log
  echo "$(date -u +%T) done $s" >> /tmp/chlift-demo/casts/progress.log
done
