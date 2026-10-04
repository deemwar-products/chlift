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
