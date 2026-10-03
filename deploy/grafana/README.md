# Grafana dashboard

`chlift-dashboard.json` reads the metrics of `chlift migrate exporter` (mirror state, replication-slot WAL, rows in
Postgres vs each replica, CDC rate, days that differ) and of the Go SDK's `WritePrometheus` (shadow mismatches,
latency by store). It expects a Prometheus datasource with uid `chlift-prom`.

Alert on these: `chlift_slot_retained_wal_bytes` growing (a stalled mirror fills the Postgres disk),
`chlift_mirror_up == 0`, `migration_reconcile_mismatch_days > 0`, `migration_shadow_mismatch_total` increasing.
