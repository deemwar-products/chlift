package migrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStub runs chTimeScript the way Host.Run does (sh -c "set -e; ..."), with a stub clickhouse-client on PATH.
func runStub(t *testing.T, stub string) (string, string, error) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clickhouse-client"), []byte("#!/bin/sh\n"+stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "set -e; "+chTimeScript("SELECT 1"))
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return strings.TrimSpace(out.String()), errb.String(), err
}

func TestChTimeKeepsElapsed(t *testing.T) {
	out, _, err := runStub(t, "echo 'some warning' >&2; echo 0.633 >&2\n")
	if err != nil || out != "0.633" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestChTimeSurfacesQueryError(t *testing.T) {
	// 2026-10-04: a Code 241 here used to come back as a ParseFloat error on the "(query: ...)" line.
	out, stderr, err := runStub(t, "echo 'Code: 241. DB::Exception: memory limit exceeded' >&2; echo '(query: SELECT 1)' >&2; exit 241\n")
	if err == nil || !strings.Contains(stderr, "Code: 241") || out != "" {
		t.Fatalf("want failure carrying Code 241, got out=%q stderr=%q err=%v", out, stderr, err)
	}
}
