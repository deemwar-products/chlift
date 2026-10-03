package cluster

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io/fs"
	"strings"

	"github.com/deemwar-products/chlift/internal/config"
)

func osMode(m uint32) fs.FileMode { return fs.FileMode(m) }

// Check is one verify result; Fix says what to do when it fails.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Info string `json:"info,omitempty"`
	Fix  string `json:"fix,omitempty"`
}

// Verify checks Keeper quorum, every replica's health, and a real cross-replica write.
func Verify(ctx context.Context, c *config.Config, nodes []*Node) []Check {
	var out []Check
	out = append(out, keeperQuorum(ctx, nodes)...)
	var chs []*Node
	for _, n := range nodes {
		if !n.clickhouse() {
			continue
		}
		chs = append(chs, n)
		v, err := SQL(ctx, n, "SELECT version()")
		out = append(out, Check{Name: n.Cfg.Name + ": clickhouse answers", OK: err == nil, Info: v + errInfo(err),
			Fix: "check the clickhouse-server log in /var/log/clickhouse-server/"})
		bad, err := SQL(ctx, n, "SELECT count() FROM system.replicas WHERE is_readonly OR is_session_expired")
		out = append(out, Check{Name: n.Cfg.Name + ": no read-only replicas", OK: err == nil && bad == "0",
			Info: "read-only replicas: " + bad + errInfo(err), Fix: "Keeper unreachable from this host; see the quorum check"})
	}
	if len(chs) > 0 {
		out = append(out, smokeTest(ctx, c, chs))
	}
	return out
}

func keeperQuorum(ctx context.Context, nodes []*Node) []Check {
	var out []Check
	var keepers, leaders, followersSynced int
	for _, n := range nodes {
		if !n.Cfg.Has(config.RoleKeeper) {
			continue
		}
		keepers++
		mntr, err := n.SSH.Run(ctx, `timeout 5 bash -c 'exec 3<>/dev/tcp/127.0.0.1/9181; printf mntr >&3; cat <&3'`)
		state := field(mntr, "zk_server_state")
		out = append(out, Check{Name: n.Cfg.Name + ": keeper answers", OK: err == nil && state != "",
			Info: "state " + state + errInfo(err), Fix: "check the keeper log and that port 9181/9234 are open between keeper hosts"})
		if state == "leader" || state == "standalone" {
			leaders++
			fmt.Sscan(field(mntr, "zk_synced_followers"), &followersSynced)
		}
	}
	want := keepers - 1
	ok := leaders == 1 && (keepers == 1 || followersSynced == want)
	out = append(out, Check{Name: "keeper quorum", OK: ok,
		Info: fmt.Sprintf("%d member(s), %d leader(s), %d/%d followers synced", keepers, leaders, followersSynced, want),
		Fix:  "a quorum needs a majority of keeper members up and able to reach each other on port 9234"})
	return out
}

// smokeTest writes a row on the first replica and reads it on every other replica.
func smokeTest(ctx context.Context, c *config.Config, chs []*Node) Check {
	var b [8]byte
	_, _ = rand.Read(b[:])
	marker := binary.BigEndian.Uint64(b[:]) >> 1
	db := "chlift_smoke"
	steps := []string{
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s ON CLUSTER %s", db, c.Cluster),
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s.t ON CLUSTER %s (x UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/%s/t', '{replica}') ORDER BY x", db, c.Cluster, db),
		fmt.Sprintf("INSERT INTO %s.t VALUES (%d)", db, marker),
	}
	check := Check{Name: "replication: insert on " + chs[0].Cfg.Name + ", read on every replica",
		Fix: "check Keeper quorum, interserver port 9009 between replicas, and the cluster secret"}
	for _, q := range steps {
		if _, err := SQL(ctx, chs[0], q); err != nil {
			check.Info = err.Error()
			return check
		}
	}
	var seen []string
	for _, n := range chs {
		got, err := SQL(ctx, n, fmt.Sprintf("SYSTEM SYNC REPLICA %s.t; SELECT count() FROM %s.t WHERE x = %d", db, db, marker))
		if err != nil || strings.TrimSpace(got) != "1" {
			check.Info = fmt.Sprintf("%s did not see the row (%q)%s", n.Cfg.Name, got, errInfo(err))
			return check
		}
		seen = append(seen, n.Cfg.Name)
	}
	_, _ = SQL(ctx, chs[0], fmt.Sprintf("DROP DATABASE %s ON CLUSTER %s SYNC", db, c.Cluster))
	check.OK = true
	check.Info = "row seen on " + strings.Join(seen, ", ")
	return check
}

func field(mntr, key string) string {
	for _, line := range strings.Split(mntr, "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func errInfo(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}
