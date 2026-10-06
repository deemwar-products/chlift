package migrate

import (
	"slices"
	"strings"
	"testing"
)

func events() pgTable {
	return pgTable{schema: "public", name: "events", rows: 1_000_000, ins: 1_000_000, pk: []string{"id"}, replIdent: "d",
		cols: []column{{"id", "bigint", "p"}, {"tenant_id", "integer", "p"}, {"event_time", "timestamp with time zone", "p"},
			{"payload", "jsonb", "x"}}}
}

func TestAppendOnlyEventsSortByTenantTimePK(t *testing.T) {
	tm, reason := propose(events(), 100_000, false)
	if reason != "" {
		t.Fatal(reason)
	}
	if !slices.Equal(tm.OrderBy, []string{"tenant_id", "event_time", "id"}) || tm.PartitionBy != "toYYYYMM(event_time)" {
		t.Errorf("order_by %v partition %q", tm.OrderBy, tm.PartitionBy)
	}
}

func TestMutatedTablesSortByPK(t *testing.T) {
	e := events()
	e.upd = 10
	tm, _ := propose(e, 100_000, false)
	if !slices.Equal(tm.OrderBy, []string{"id"}) || tm.PartitionBy != "" || !tm.Mutated {
		t.Errorf("mutated table must sort by PK only: %v %q", tm.OrderBy, tm.PartitionBy)
	}
}

func TestNoTenantColumnDoesNotPickTime(t *testing.T) {
	e := events()
	e.cols = []column{{"id", "bigint", "p"}, {"created_at", "timestamp without time zone", "p"}}
	tm, _ := propose(e, 100_000, false)
	if !slices.Equal(tm.OrderBy, []string{"created_at", "id"}) {
		t.Errorf("got %v", tm.OrderBy)
	}
}

func TestSkips(t *testing.T) {
	e := events()
	e.pk = nil
	if _, r := propose(e, 1, false); r == "" {
		t.Error("table without PK must be skipped")
	}
	e = events()
	e.upd, e.ins = 500_000, 1_000_000
	if _, r := propose(e, 1, false); r == "" {
		t.Error("entity-like table must be skipped unless named explicitly")
	}
	if _, r := propose(e, 1, true); r != "" {
		t.Error("explicitly named table must be planned")
	}
}

func TestTimeFirstKeyNeedsFullIdentity(t *testing.T) {
	// Found on dev data: delete markers carried logged_at=1970-01-01 and never folded the deleted rows.
	e := events()
	tm, _ := propose(e, 1, false)
	if needsFullIdentity(e, tm) == "" {
		t.Error("a time-first sort key must require REPLICA IDENTITY FULL")
	}
	e.upd = 10
	e.cols = []column{{"id", "bigint", "p"}, {"event_time", "timestamp with time zone", "p"}}
	tm, _ = propose(e, 1, false)
	if needsFullIdentity(e, tm) != "" {
		t.Error("PK-first key on a table without TOAST-able columns needs no full identity")
	}
}

// Found in the 1.0.0 -> 1.0.1 upgrade test: a users table with zero update counters (freshly loaded) got
// ORDER BY (account_id, updated_at, id); every UPDATE then left the old row live in ClickHouse.
func TestUpdatedAtNeverLeadsTheSortKey(t *testing.T) {
	users := pgTable{schema: "public", name: "users", rows: 200_000, ins: 200_000, pk: []string{"id"}, replIdent: "d",
		cols: []column{{"id", "bigint", "p"}, {"account_id", "bigint", "p"}, {"email", "text", "x"},
			{"updated_at", "timestamp with time zone", "p"}}}
	if _, r := propose(users, 100_000, false); !strings.Contains(r, "only updated_at, which updates rewrite") {
		t.Errorf("auto-plan must skip it and say why, got %q", r)
	}
	tm, r := propose(users, 100_000, true)
	if r != "" {
		t.Fatal(r)
	}
	if !slices.Equal(tm.OrderBy, []string{"id"}) || tm.PartitionBy != "" {
		t.Errorf("named explicitly, it must sort by primary key only: order_by %v partition %q", tm.OrderBy, tm.PartitionBy)
	}
}

func TestPickTimeSkipsRewrittenColumns(t *testing.T) {
	for name, want := range map[string]bool{
		"updated_at": true, "modified_at": true, "last_seen_at": true, "date_last": true, "row_updated": true,
		"deleted_at": true, "created_at": false, "event_time": false, "logged_at": false, "update_count_at": false,
	} {
		if got := rewrittenOnUpdate(name); got != want {
			t.Errorf("%s: rewrittenOnUpdate=%v, want %v", name, got, want)
		}
	}
	// A real event time still wins over an updated_at that comes first.
	cols := []column{{"updated_at", "timestamp", "p"}, {"occurred", "timestamp", "p"}}
	if got := pickTime(cols); got != "occurred" {
		t.Errorf("pickTime = %q, want occurred", got)
	}
}

// Tested 10-07: moving 500 request_logs rows to another account left 500 duplicates (migrate check failed on it).
// The tenant-first key is kept for per-tenant query speed, so the plan must say so.
func TestTenantFirstKeyWarnsAboutTenantMoves(t *testing.T) {
	tm, _ := propose(events(), 100_000, false)
	found := false
	for _, n := range tm.Notes {
		if strings.Contains(n, "changes a row's tenant_id leaves the old row live") {
			found = true
		}
	}
	if !found {
		t.Errorf("tenant-first key without the tenant-move warning: %v", tm.Notes)
	}
}
