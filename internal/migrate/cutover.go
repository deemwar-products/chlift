package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deemwar-products/chlift/internal/peerdb"
)

// Cutover hands the tables to ClickHouse. Event writes to Postgres must already be stopped
// (the app is in clickhouse_only mode, or the writers are paused):
//  1. record the Postgres WAL position W;
//  2. wait until PeerDB has confirmed W and its next normalize has run;
//  3. prove day counts and PK-range checksums are equal on every replica (else stop, change nothing);
//  4. stop the mirror, keeping the ClickHouse tables; PeerDB drops its replication slot;
//  5. drop the publication. Postgres tables are left in place.
func Cutover(ctx context.Context, e Env, p *Plan) error {
	conn, err := pgx.Connect(ctx, e.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var w string
	if err := conn.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&w); err != nil {
		return err
	}
	slot := "peerflow_slot_" + p.Mirror
	fmt.Fprintf(e.Log, "cutover: watermark %s; waiting for PeerDB to confirm it\n", w)
	deadline := time.Now().Add(15 * time.Minute)
	for {
		var done bool
		err := conn.QueryRow(ctx, "SELECT confirmed_flush_lsn >= $1::pg_lsn FROM pg_replication_slots WHERE slot_name = $2", w, slot).Scan(&done)
		if err != nil {
			return fmt.Errorf("replication slot %s: %w", slot, err)
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PeerDB has not confirmed %s after 15 minutes; are writes to the event tables really stopped?", w)
		}
		time.Sleep(5 * time.Second)
	}
	time.Sleep(30 * time.Second) // one idle timeout (10 s) plus margin for the last normalize
	res, err := Check(ctx, e, p)
	if err != nil {
		return err
	}
	for _, r := range res {
		if !r.OK || len(r.Today) > 0 {
			return fmt.Errorf("cutover stopped, nothing changed: %s differs (%v %v %s)", r.Table, r.BadDays, r.Today, r.Err)
		}
	}
	sums, err := Checksums(ctx, e, p)
	if err != nil {
		return err
	}
	for tbl, bad := range sums {
		if len(bad) > 0 && bad[0] != "skipped: checksums need a single-column integer primary key" {
			return fmt.Errorf("cutover stopped, nothing changed: %s checksums differ: %v", tbl, bad)
		}
	}
	fmt.Fprintln(e.Log, "cutover: Postgres and every replica agree (day counts and PK-range checksums)")
	if _, err := peerdb.Call(ctx, e.Peer, "POST", "/v1/mirrors/state_change", map[string]any{
		"flow_job_name": p.Mirror, "requested_flow_state": "STATUS_TERMINATING", "skip_destination_drop": true}); err != nil {
		return err
	}
	for i := 0; ; i++ {
		if _, err := mirrorStatus(ctx, e, p.Mirror); err != nil {
			break
		}
		if i > 120 {
			return fmt.Errorf("mirror %s still running after 10 minutes", p.Mirror)
		}
		time.Sleep(5 * time.Second)
	}
	dropPeers(ctx, e, p)
	if _, err := conn.Exec(ctx, "DROP PUBLICATION IF EXISTS "+quoteIdent(p.Publication)); err != nil {
		return err
	}
	var slots int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = $1", slot).Scan(&slots); err != nil {
		return err
	}
	if slots > 0 {
		return fmt.Errorf("mirror stopped but slot %s still exists: drop it with SELECT pg_drop_replication_slot('%s') once PeerDB is down", slot, slot)
	}
	fmt.Fprintf(e.Log, "cutover done: mirror stopped, slot and publication dropped; ClickHouse database %s keeps the data. Postgres tables are untouched (archive or drop them when you are ready)\n", p.Database)
	return nil
}
