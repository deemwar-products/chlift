package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deemwar-products/chlift/internal/cluster"
	"github.com/deemwar-products/chlift/internal/config"
	"github.com/deemwar-products/chlift/internal/peerdb"
	"github.com/deemwar-products/chlift/internal/remote"
)

// Peers are named per mirror. PeerDB peers are shared by reference: reusing one name across mirrors let a new
// mirror's settings (its ClickHouse database) silently retarget every older mirror that used the same peer.
func srcPeer(p *Plan) string { return p.Mirror + "_src" }
func dstPeer(p *Plan) string { return p.Mirror + "_ch" }

// Env is everything a migrate command needs.
type Env struct {
	Cfg   *config.Config
	DSN   string
	CH    []*cluster.Node // ClickHouse replicas
	Peer  *remote.Host
	Peers config.Host
	Log   io.Writer
}

// Start prepares Postgres and ClickHouse, then creates the PeerDB peers and the mirror (snapshot + CDC).
// Postgres changes run only with applyPG; without it, Start stops and prints the exact SQL.
func Start(ctx context.Context, e Env, p *Plan, applyPG bool) error {
	if err := preparePostgres(ctx, e, p, applyPG); err != nil {
		return err
	}
	for _, n := range e.CH {
		q := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s ENGINE = Replicated('/clickhouse/databases/%s', '{shard}', '{replica}')", p.Database, p.Database)
		if _, err := cluster.SQL(ctx, n, q); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.Log, "clickhouse: database %s (Replicated) on %d replica(s)\n", p.Database, len(e.CH))
	if _, err := mirrorStatus(ctx, e, p.Mirror); err == nil {
		fmt.Fprintf(e.Log, "peerdb: mirror %s already exists; nothing to start\n", p.Mirror)
		return nil
	}
	src, err := pgx.ParseConfig(e.DSN)
	if err != nil {
		return err
	}
	host, port := src.Host, int(src.Port)
	if e.Cfg.Postgres.PeerHost != "" {
		host = e.Cfg.Postgres.PeerHost
	}
	if e.Cfg.Postgres.PeerPort != 0 {
		port = e.Cfg.Postgres.PeerPort
	}
	chPW, err := config.Secret(config.EnvCHPassword)
	if err != nil {
		return err
	}
	peers := []map[string]any{
		{"name": srcPeer(p), "type": "POSTGRES", "postgres_config": map[string]any{
			"host": host, "port": port, "user": src.User, "password": src.Password, "database": src.Database}},
		// PeerDB writes through one pinned replica, with replicated=true and no cluster: no Distributed table
		// (PeerDB #4746 / ClickHouse #97557). The Replicated database copies DDL and data to every replica.
		{"name": dstPeer(p), "type": "CLICKHOUSE", "clickhouse_config": map[string]any{
			"host": e.CH[0].Cfg.Private(), "port": 9000, "user": "default", "password": chPW, "database": p.Database,
			"disable_tls": true, "replicated": true}},
	}
	for _, pr := range peers {
		if _, err := peerdb.Call(ctx, e.Peer, "POST", "/v1/peers/create", map[string]any{"peer": pr, "allow_update": false}); err != nil {
			return err
		}
	}
	var mappings []map[string]any
	for _, t := range p.Tables {
		var cols []map[string]any
		for i, c := range t.OrderBy {
			cols = append(cols, map[string]any{"source_name": c, "ordering": i + 1})
		}
		m := map[string]any{"source_table_identifier": t.Source, "destination_table_identifier": t.Target, "columns": cols}
		if t.PartitionBy != "" {
			m["partition_by_expr"] = t.PartitionBy
		}
		mappings = append(mappings, m)
	}
	resp, err := peerdb.Call(ctx, e.Peer, "POST", "/v1/flows/cdc/create", map[string]any{"connection_configs": map[string]any{
		"flow_job_name": p.Mirror, "source_name": srcPeer(p), "destination_name": dstPeer(p), "publication_name": p.Publication,
		"table_mappings": mappings, "do_initial_snapshot": true, "max_batch_size": 100000, "idle_timeout_seconds": 10,
		"snapshot_num_rows_per_partition": 100000, "snapshot_max_parallel_workers": 2, "snapshot_num_tables_in_parallel": 2,
		"soft_delete_col_name": "_peerdb_is_deleted", "synced_at_col_name": "_peerdb_synced_at"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Log, "peerdb: mirror %s created (%s); snapshot then CDC. Follow with `chlift migrate status`\n", p.Mirror, strings.TrimSpace(string(resp)))
	return nil
}

func preparePostgres(ctx context.Context, e Env, p *Plan, apply bool) error {
	conn, err := pgx.Connect(ctx, e.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var wal, keep string
	if err := conn.QueryRow(ctx, "SELECT current_setting('wal_level'), current_setting('max_slot_wal_keep_size')").Scan(&wal, &keep); err != nil {
		return err
	}
	if wal != "logical" {
		return fmt.Errorf("postgres wal_level is %q; run ALTER SYSTEM SET wal_level = logical; then restart Postgres", wal)
	}
	if keep == "-1" {
		// RDS/Aurora forbid ALTER SYSTEM; their settings live in the DB parameter group.
		var rds bool
		_ = conn.QueryRow(ctx, "SELECT current_setting('rds.logical_replication', true) IS NOT NULL").Scan(&rds)
		how := "ALTER SYSTEM SET max_slot_wal_keep_size = '50GB'; SELECT pg_reload_conf()"
		if rds {
			how = "set max_slot_wal_keep_size in the DB parameter group (RDS/Aurora do not allow ALTER SYSTEM)"
		}
		fmt.Fprintf(e.Log, "warning: max_slot_wal_keep_size is unlimited: a stalled mirror can fill the Postgres disk (%s)\n", how)
	}
	var missing []string
	for _, fix := range p.PostgresFixes {
		done, err := applied(ctx, conn, p, fix)
		if err != nil {
			return err
		}
		if !done {
			missing = append(missing, fix)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !apply {
		return fmt.Errorf("postgres needs these changes first (rerun with --apply-postgres, or run them yourself):\n  %s;", strings.Join(missing, ";\n  "))
	}
	for _, fix := range missing {
		if _, err := conn.Exec(ctx, fix); err != nil {
			return fmt.Errorf("%s: %w", fix, err)
		}
		fmt.Fprintln(e.Log, "postgres:", fix)
	}
	return nil
}

// applied reports whether a planned fix is already in effect.
func applied(ctx context.Context, conn *pgx.Conn, p *Plan, fix string) (bool, error) {
	var n int
	switch {
	case strings.HasPrefix(fix, "CREATE PUBLICATION"):
		err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_publication WHERE pubname = $1", p.Publication).Scan(&n)
		return n > 0, err
	case strings.HasSuffix(fix, "REPLICA IDENTITY FULL"):
		tbl := strings.TrimSuffix(strings.TrimPrefix(fix, "ALTER TABLE "), " REPLICA IDENTITY FULL")
		err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE oid = $1::regclass AND relreplident = 'f'", tbl).Scan(&n)
		return n > 0, err
	}
	return false, nil
}

// Status is the mirror state plus the replication slot's retained WAL on Postgres.
type Status struct {
	Tables     []TableProgress `json:"tables"`
	Mirror     string          `json:"mirror"`
	State      string          `json:"state"`
	RowsSynced int64           `json:"rows_synced"`
	SlotBytes  int64           `json:"slot_retained_wal_bytes"`
	Snapshot   json.RawMessage `json:"snapshot,omitempty"`
}

// TableProgress is a cheap progress view: Postgres's row estimate and each replica's row count (all versions).
type TableProgress struct {
	Table      string           `json:"table"`
	PGEstimate int64            `json:"postgres_estimate"`
	Replicas   map[string]int64 `json:"replica_rows"`
}

func GetStatus(ctx context.Context, e Env, p *Plan) (*Status, error) {
	raw, err := mirrorStatus(ctx, e, p.Mirror)
	if err != nil {
		// PeerDB cannot always read flow details from a busy workflow; the state alone is still useful.
		raw, err = peerdb.Call(ctx, e.Peer, "POST", "/v1/mirrors/status",
			map[string]any{"flow_job_name": p.Mirror, "include_flow_info": false, "exclude_batches": true})
		if err != nil {
			return nil, err
		}
	}
	var r struct {
		State string `json:"currentFlowState"`
		CDC   struct {
			RowsSynced json.Number     `json:"rowsSynced"`
			Snapshot   json.RawMessage `json:"snapshotStatus"`
		} `json:"cdcStatus"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	s := &Status{Mirror: p.Mirror, State: strings.TrimPrefix(r.State, "STATUS_"), Snapshot: r.CDC.Snapshot}
	s.RowsSynced, _ = r.CDC.RowsSynced.Int64()
	conn, err := pgx.Connect(ctx, e.DSN)
	if err != nil {
		return s, err
	}
	defer conn.Close(ctx)
	err = conn.QueryRow(ctx, `SELECT coalesce(max(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)),0)::bigint
		FROM pg_replication_slots WHERE slot_name = $1`, "peerflow_slot_"+p.Mirror).Scan(&s.SlotBytes)
	if err != nil {
		return s, err
	}
	for _, t := range p.Tables {
		tp := TableProgress{Table: t.Source, Replicas: map[string]int64{}}
		_ = conn.QueryRow(ctx, "SELECT greatest(reltuples,0)::bigint FROM pg_class WHERE oid = $1::regclass", t.Source).Scan(&tp.PGEstimate)
		for _, n := range e.CH {
			out, err := cluster.SQL(ctx, n, fmt.Sprintf("SELECT coalesce(sum(total_rows),0) FROM system.tables WHERE database = '%s' AND name = '%s'", p.Database, t.Target))
			if err == nil {
				var v int64
				fmt.Sscan(out, &v)
				tp.Replicas[n.Cfg.Name] = v
			}
		}
		s.Tables = append(s.Tables, tp)
	}
	return s, nil
}

func mirrorStatus(ctx context.Context, e Env, mirror string) (json.RawMessage, error) {
	return peerdb.Call(ctx, e.Peer, "POST", "/v1/mirrors/status",
		map[string]any{"flow_job_name": mirror, "include_flow_info": true, "exclude_batches": true})
}

// Abort stops the mirror (PeerDB drops its slot), drops the ClickHouse database on every replica and the
// publication chlift created. Postgres tables are never touched.
func Abort(ctx context.Context, e Env, p *Plan) error {
	if _, err := mirrorStatus(ctx, e, p.Mirror); err == nil {
		// The workflow acts on TERMINATING; the API also accepts TERMINATED but the running workflow ignores it.
		if _, err := peerdb.Call(ctx, e.Peer, "POST", "/v1/mirrors/state_change", map[string]any{
			"flow_job_name": p.Mirror, "requested_flow_state": "STATUS_TERMINATING"}); err != nil {
			return err
		}
		gone := 0
		for i := 0; i < 120 && gone < 2; i++ { // PeerDB drops the slot and its tables, then forgets the mirror
			time.Sleep(5 * time.Second)
			if _, err := mirrorStatus(ctx, e, p.Mirror); err != nil {
				gone++
			} else {
				gone = 0
			}
		}
		if gone < 2 {
			return fmt.Errorf("mirror %s still exists after 10 minutes; nothing else was dropped", p.Mirror)
		}
		fmt.Fprintf(e.Log, "peerdb: mirror %s dropped\n", p.Mirror)
	}
	dropPeers(ctx, e, p)
	for _, n := range e.CH {
		if _, err := cluster.SQL(ctx, n, fmt.Sprintf("DROP DATABASE IF EXISTS %s SYNC", p.Database)); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.Log, "clickhouse: database %s dropped on %d replica(s)\n", p.Database, len(e.CH))
	conn, err := pgx.Connect(ctx, e.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP PUBLICATION IF EXISTS "+quoteIdent(p.Publication)); err != nil {
		return err
	}
	var slots int
	_ = conn.QueryRow(ctx, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = $1", "peerflow_slot_"+p.Mirror).Scan(&slots)
	fmt.Fprintf(e.Log, "postgres: publication %s dropped; mirror slots left: %d\n", p.Publication, slots)
	return nil
}

// dropPeers removes this mirror's own peers (best effort: a peer may already be gone).
func dropPeers(ctx context.Context, e Env, p *Plan) {
	for _, name := range []string{srcPeer(p), dstPeer(p)} {
		if _, err := peerdb.Call(ctx, e.Peer, "POST", "/v1/peers/drop", map[string]any{"peer_name": name}); err == nil {
			fmt.Fprintf(e.Log, "peerdb: peer %s dropped\n", name)
		}
	}
}
