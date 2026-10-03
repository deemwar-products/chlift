package migrate

import (
	"slices"
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
