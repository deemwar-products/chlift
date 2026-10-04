# Grafana dashboard

`chlift-dashboard.json` reads the metrics of `chlift migrate exporter` (mirror state, replication-slot WAL, rows in
Postgres vs each replica, CDC rate, days that differ) and of the Go SDK's `WritePrometheus` (shadow mismatches,
latency by store). It expects a Prometheus datasource with uid `chlift-prom`.

Alert on these: `chlift_slot_retained_wal_bytes` growing (a stalled mirror fills the Postgres disk),
`chlift_mirror_up == 0`, `migration_reconcile_mismatch_days > 0`, `migration_shadow_mismatch_total` increasing.

The SDK panels stay empty until Prometheus scrapes an application that serves `WritePrometheus`. To see them fill
without your own app, run the example in metrics mode. It shadow-reads its three correct queries every 10 seconds
and serves `/metrics`:

```sh
CHLIFT_PG_DSN=... CHLIFT_CH_DSN=... CHLIFT_METRICS_ADDR=127.0.0.1:9466 go run ./sdk/go/example
```

Then add `127.0.0.1:9466` as a Prometheus scrape target.
