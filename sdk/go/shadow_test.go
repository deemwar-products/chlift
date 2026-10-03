package shadow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func fixed(rows [][]any, err error, calls *int, mu *sync.Mutex) Runner {
	return func(context.Context, string, ...any) ([][]any, error) {
		mu.Lock()
		*calls++
		mu.Unlock()
		return rows, err
	}
}

func TestModesRouteReads(t *testing.T) {
	var mu sync.Mutex
	var pgCalls, chCalls int
	pg := fixed([][]any{{"pg"}}, nil, &pgCalls, &mu)
	ch := fixed([][]any{{"ch"}}, nil, &chCalls, &mu)
	for mode, want := range map[Mode]string{PostgresOnly: "pg", ClickHouseOnly: "ch", ShadowRead: "pg"} {
		c := &Client{Postgres: pg, ClickHouse: ch, Mode: mode, SampleRate: -1} // -1: never shadow
		rows, err := c.Read(context.Background(), Query{Name: "q"})
		if err != nil || rows[0][0] != want {
			t.Errorf("%s: got %v %v, want %s", mode, rows, err, want)
		}
	}
}

func TestShadowNeverChangesTheAnswerAndCountsMismatch(t *testing.T) {
	var mu sync.Mutex
	var n int
	got := make(chan Mismatch, 1)
	c := &Client{Mode: ShadowRead, SampleRate: 1,
		Postgres:   fixed([][]any{{int64(1), "a"}}, nil, &n, &mu),
		ClickHouse: fixed([][]any{{uint64(2), "a"}}, nil, &n, &mu),
		OnMismatch: func(m Mismatch) { got <- m }}
	rows, err := c.Read(context.Background(), Query{Name: "dau"})
	if err != nil || rows[0][0] != int64(1) {
		t.Fatalf("shadow_read must return the Postgres answer, got %v %v", rows, err)
	}
	select {
	case m := <-got:
		if m.Query != "dau" || m.FirstDifferentRow != 0 {
			t.Errorf("mismatch %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no mismatch reported")
	}
	if s := c.Stats(); s.Mismatches != 1 || s.Shadowed != 1 {
		t.Errorf("stats %+v", s)
	}
}

func TestShadowErrorIsCountedNotReturned(t *testing.T) {
	var mu sync.Mutex
	var n int
	c := &Client{Mode: ShadowRead, SampleRate: 1,
		Postgres:   fixed([][]any{{1}}, nil, &n, &mu),
		ClickHouse: fixed(nil, errors.New("clickhouse down"), &n, &mu)}
	if _, err := c.Read(context.Background(), Query{}); err != nil {
		t.Fatalf("a ClickHouse failure must never fail the read: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().ShadowErrors != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Stats().ShadowErrors != 1 {
		t.Error("shadow error not counted")
	}
}

func TestNormalisationAcrossStores(t *testing.T) {
	pgT := time.Date(2026, 10, 1, 5, 30, 0, 123456789, time.FixedZone("IST", 19800))
	chT := time.Date(2026, 10, 1, 0, 0, 0, 123456000, time.UTC)
	pg := [][]any{{pgT, []byte("12.50"), int64(7), 0.1 + 0.2, nil, true}}
	ch := [][]any{{chT, float64(12.5), uint32(7), 0.3, nil, uint8(1)}}
	if m, same := Compare(Query{Name: "n"}, pg, ch); !same {
		t.Errorf("equal answers must compare equal: %+v", m)
	}
}

func TestUnorderedIsASetOrderedIsNot(t *testing.T) {
	a := [][]any{{"x"}, {"y"}}
	b := [][]any{{"y"}, {"x"}}
	if _, same := Compare(Query{}, a, b); !same {
		t.Error("unordered results with the same rows must match")
	}
	if _, same := Compare(Query{Ordered: true}, a, b); same {
		t.Error("ordered results in a different order must not match")
	}
	if m, same := Compare(Query{}, a, a[:1]); same || m.PostgresRows != 2 || m.ClickHouseRows != 1 {
		t.Errorf("row count difference must be reported: %+v", m)
	}
}
