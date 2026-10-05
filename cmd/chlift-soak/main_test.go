package main

import (
	"errors"
	"strings"
	"testing"
)

type fakeRows struct {
	row []string
	err error
}

func (f *fakeRows) Next() bool {
	if f.row == nil {
		return false
	}
	return true
}

func (f *fakeRows) Scan(dst ...any) error {
	for i, d := range dst {
		*d.(*string) = f.row[i]
	}
	f.row = nil
	return nil
}

func (f *fakeRows) Err() error { return f.err }

func TestFirstRowKeepsStreamError(t *testing.T) {
	// Cycle 72 and 82 of the 2026-10-03 soak: ClickHouse Code 241 mid-stream was logged only as "no rows".
	_, err := firstRow(&fakeRows{err: errors.New("code: 241, memory limit exceeded")}, 4)
	if err == nil || !strings.Contains(err.Error(), "241") {
		t.Fatalf("want the stream error, got %v", err)
	}
}

func TestFirstRowEmpty(t *testing.T) {
	if _, err := firstRow(&fakeRows{}, 1); err == nil || err.Error() != "no rows" {
		t.Fatalf("want no rows, got %v", err)
	}
}

func TestFirstRowJoins(t *testing.T) {
	got, err := firstRow(&fakeRows{row: []string{"1", "2", "3"}}, 3)
	if err != nil || got != "1|2|3" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSelectVariants(t *testing.T) {
	all := []variant{{Name: "safe"}, {Name: "clusq"}, {Name: "clusnq"}}
	if got := selectVariants(all, ""); len(got) != 3 {
		t.Fatalf("empty list keeps all, got %v", variantNames(got))
	}
	if got := selectVariants(all, " safe "); len(got) != 1 || got[0].Name != "safe" {
		t.Fatalf("want [safe], got %v", variantNames(got))
	}
	if got := selectVariants(all, "clusnq,safe"); len(got) != 2 || got[0].Name != "clusnq" {
		t.Fatalf("want [clusnq safe], got %v", variantNames(got))
	}
}

func TestSelectSingle(t *testing.T) {
	got := selectVariants(variants, "single")
	if len(got) != 1 || got[0].Name != "single" || len(got[0].Replicas) != 1 || got[0].Replicas[0] != "ch1:9000" {
		t.Fatalf("want the single-server layout on ch1 only, got %+v", got)
	}
}
