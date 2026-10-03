package migrate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Exporter serves Prometheus metrics for one migration. Status is read on every scrape (cheap);
// the full per-day check runs on its own interval, because it scans every table.
type Exporter struct {
	Env           Env
	Plan          *Plan
	CheckInterval time.Duration

	mu           sync.Mutex
	runningSince time.Time
	lastCheck    []TableCheck
	checkedAt    time.Time
	checkErr     error
}

func (x *Exporter) Run(ctx context.Context, listen string) error {
	go x.checkLoop(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		x.write(r.Context(), w)
	})
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return srv.ListenAndServe()
}

func (x *Exporter) checkLoop(ctx context.Context) {
	every := x.CheckInterval
	if every == 0 {
		every = 10 * time.Minute
	}
	for {
		// Counting days mid-snapshot reports every unfinished day as different. Only check a mirror that has been
		// RUNNING for 2 minutes: PeerDB reports RUNNING slightly before the last snapshot rows are visible.
		s, err := GetStatus(ctx, x.Env, x.Plan)
		running := err == nil && s.State == "RUNNING"
		if !running {
			x.runningSince = time.Time{}
		} else if x.runningSince.IsZero() {
			x.runningSince = time.Now()
		}
		if running && time.Since(x.runningSince) >= 2*time.Minute {
			res, err := Check(ctx, x.Env, x.Plan)
			x.mu.Lock()
			x.lastCheck, x.checkErr, x.checkedAt = res, err, time.Now()
			x.mu.Unlock()
		} else {
			x.mu.Lock()
			x.lastCheck, x.checkedAt = nil, time.Time{}
			x.mu.Unlock()
		}
		wait := every
		if x.checkedAt.IsZero() {
			wait = 30 * time.Second // not RUNNING yet: look again soon
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (x *Exporter) write(ctx context.Context, w io.Writer) {
	m := map[string][]string{}
	add := func(name, help, labels string, v float64) {
		if _, ok := m[name]; !ok {
			m[name] = []string{"# HELP " + name + " " + help, "# TYPE " + name + " gauge"}
		}
		m[name] = append(m[name], fmt.Sprintf("%s%s %g", name, labels, v))
	}
	mirror := fmt.Sprintf(`{mirror=%q}`, x.Plan.Mirror)
	s, err := GetStatus(ctx, x.Env, x.Plan)
	if err != nil {
		add("chlift_mirror_up", "1 when PeerDB reports the mirror RUNNING", mirror, 0)
	} else {
		up := 0.0
		if s.State == "RUNNING" {
			up = 1
		}
		add("chlift_mirror_up", "1 when PeerDB reports the mirror RUNNING", mirror, up)
		add("chlift_slot_retained_wal_bytes", "WAL the replication slot keeps on Postgres; alert before the disk fills", mirror, float64(s.SlotBytes))
		add("chlift_cdc_rows_synced", "rows PeerDB has applied by CDC since the mirror started", mirror, float64(s.RowsSynced))
		for _, t := range s.Tables {
			add("chlift_table_rows", "Postgres estimate vs physical rows on each replica (includes row versions)",
				fmt.Sprintf(`{table=%q,store="postgres"}`, t.Table), float64(t.PGEstimate))
			for node, n := range t.Replicas {
				add("chlift_table_rows", "", fmt.Sprintf(`{table=%q,store=%q}`, t.Table, node), float64(n))
			}
		}
	}
	x.mu.Lock()
	res, cerr, at := x.lastCheck, x.checkErr, x.checkedAt
	x.mu.Unlock()
	if !at.IsZero() {
		add("chlift_check_age_seconds", "seconds since the last per-day check finished", mirror, time.Since(at).Seconds())
		ok := 1.0
		if cerr != nil {
			ok = 0
		}
		add("chlift_check_ran_ok", "1 when the last check could run", mirror, ok)
		for _, r := range res {
			add("migration_reconcile_mismatch_days", "days whose live row counts differ between Postgres and any replica (settled days only)",
				fmt.Sprintf(`{table=%q}`, r.Table), float64(len(r.BadDays)))
		}
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(w, strings.Join(m[n], "\n"))
	}
}
