// chlift moves Postgres event tables into a self-hosted, replicated ClickHouse cluster.
//
//	chlift init --cluster prod --host a=10.0.0.1:clickhouse,keeper --host b=10.0.0.2:clickhouse,keeper --host c=10.0.0.3:keeper
//	chlift plan      # probe hosts, show what goes where; touches nothing
//	chlift install   # converge every host (idempotent)
//	chlift verify    # keeper quorum, replica health, a real cross-replica write
//
// Every command takes --config (default chlift.yaml) and --json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deemwar-products/chlift/internal/cluster"
	"github.com/deemwar-products/chlift/internal/config"
	"github.com/deemwar-products/chlift/internal/migrate"
	"github.com/deemwar-products/chlift/internal/peerdb"
)

var version = "dev"

type hostFlags []string

func (h *hostFlags) String() string     { return strings.Join(*h, " ") }
func (h *hostFlags) Set(v string) error { *h = append(*h, v); return nil }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "plan":
		err = withNodes(ctx, args, cmdPlan)
	case "install":
		err = withNodes(ctx, args, cmdInstall)
	case "verify":
		err = withNodes(ctx, args, cmdVerify)
	case "migrate":
		err = cmdMigrate(ctx, args)
	case "seed":
		err = cmdSeed(ctx, args)
	case "version":
		fmt.Println("chlift", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "chlift:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: chlift <init|plan|install|verify|migrate|seed|version> [flags]
       chlift migrate <plan|start|status|check|bench|cutover|abort|exporter> [flags]
chlift is free and MIT licensed. Paid installation, support and training: io@deemwar.com`)
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", "chlift.yaml", "config file to write")
	name := fs.String("cluster", "chlift", "cluster name")
	chv := fs.String("clickhouse-version", "26.3.39.7", "ClickHouse build to pin (the one v1.0.0 was soaked on), or an LTS line such as 26.3 for its newest build")
	user := fs.String("ssh-user", "root", "SSH user (non-root uses sudo -n)")
	port := fs.Int("ssh-port", 22, "SSH port")
	key := fs.String("ssh-key", "", "SSH private key (default: ssh-agent)")
	insecure := fs.Bool("insecure-ignore-host-key", false, "skip host key checks (dev containers only)")
	pgEnv := fs.String("pg-dsn-env", "CHLIFT_PG_DSN", "env var holding the Postgres DSN")
	secretsFile := fs.String("secrets-file", "", "where generated secrets go (mode 0600); default ~/.config/chlift/<cluster>.env")
	pgPeerHost := fs.String("pg-peer-host", "", "Postgres host as PeerDB reaches it, when it differs from the DSN")
	pgPeerPort := fs.Int("pg-peer-port", 0, "Postgres port as PeerDB reaches it")
	pdbDir := fs.String("peerdb-dir", "", "PeerDB compose dir on the peerdb host (default /opt/chlift/peerdb)")
	pdbNet := fs.String("peerdb-network", "", "join an existing docker network (dev environments)")
	s3End := fs.String("s3-endpoint", "", "staging S3 URL as ClickHouse and PeerDB reach it (default http://<peerdb host>:8333)")
	var hosts hostFlags
	fs.Var(&hosts, "host", "name=address[:port][@private]:role,role (repeat); roles: clickhouse, keeper, peerdb; address 'local' = this machine")
	fs.Parse(args)
	c := &config.Config{Version: 1, Cluster: *name, ClickHouseVersion: *chv,
		SSH:      config.SSH{User: *user, Port: *port, KeyFile: *key, InsecureIgnoreHostKey: *insecure},
		Postgres: config.Postgres{DSNEnv: *pgEnv, PeerHost: *pgPeerHost, PeerPort: *pgPeerPort},
		PeerDB:   config.PeerDB{Dir: *pdbDir, Network: *pdbNet, S3Endpoint: *s3End}}
	for _, h := range hosts {
		hh, err := parseHost(h)
		if err != nil {
			return err
		}
		c.Hosts = append(c.Hosts, hh)
	}
	errs, warns := c.Validate()
	for _, w := range warns {
		fmt.Println("warning:", w)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	c.SecretsFile = *secretsFile
	if c.SecretsFile == "" {
		c.SecretsFile = config.DefaultSecretsFile(c.Cluster)
	}
	created, err := config.EnsureSecrets(c.SecretsFile)
	if err != nil {
		return err
	}
	if len(created) > 0 {
		fmt.Printf("generated %s in %s (mode 0600; values not shown)\n", strings.Join(created, ", "), c.SecretsFile)
	}
	if err := config.Save(*path, c); err != nil {
		return err
	}
	fmt.Println("wrote", *path)
	return nil
}

// parseHost reads name=address[:sshport][@private]:roles.
func parseHost(s string) (config.Host, error) {
	name, rest, ok := strings.Cut(s, "=")
	i := strings.LastIndex(rest, ":")
	if !ok || i < 0 {
		return config.Host{}, fmt.Errorf("--host %q: want name=address[:sshport][@private]:roles", s)
	}
	addr, roles := rest[:i], rest[i+1:]
	h := config.Host{Name: name, Roles: strings.Split(roles, ",")}
	addr, h.PrivateAddress, _ = strings.Cut(addr, "@")
	if a, p, ok := strings.Cut(addr, ":"); ok {
		addr = a
		fmt.Sscan(p, &h.Port)
	}
	h.Address = addr
	return h, nil
}

type nodesCmd func(ctx context.Context, c *config.Config, nodes []*cluster.Node, asJSON bool) error

func withNodes(ctx context.Context, args []string, f nodesCmd) error {
	fs := flag.NewFlagSet("cmd", flag.ExitOnError)
	path := fs.String("config", "chlift.yaml", "config file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if errs, _ := c.Validate(); len(errs) > 0 {
		return fmt.Errorf("%s: %s", *path, strings.Join(errs, "; "))
	}
	// Secrets added by newer chlift versions are generated on first use (values never printed).
	if created, err := config.EnsureSecrets(c.SecretsFile); err != nil {
		return err
	} else if len(created) > 0 {
		fmt.Fprintf(os.Stderr, "generated %s in %s\n", strings.Join(created, ", "), c.SecretsFile)
	}
	nodes, err := cluster.Connect(ctx, c)
	if err != nil {
		return err
	}
	defer cluster.Close(nodes)
	return f(ctx, c, nodes, *asJSON)
}

func cmdPlan(_ context.Context, c *config.Config, nodes []*cluster.Node, asJSON bool) error {
	type hostPlan struct {
		Name     string        `json:"name"`
		Roles    []string      `json:"roles"`
		Facts    cluster.Facts `json:"facts"`
		Installs []string      `json:"installs"`
		Warnings []string      `json:"warnings,omitempty"`
	}
	_, warns := c.Validate()
	var plan []hostPlan
	for _, n := range nodes {
		p := hostPlan{Name: n.Cfg.Name, Roles: n.Cfg.Roles, Facts: n.Facts}
		switch {
		case n.Cfg.Has(config.RoleClickHouse) && n.Cfg.Has(config.RoleKeeper):
			p.Installs = append(p.Installs, "clickhouse-server "+c.ClickHouseVersion+".x (embedded keeper id "+fmt.Sprint(c.KeeperID(n.Cfg.Name))+")")
		case n.Cfg.Has(config.RoleClickHouse):
			p.Installs = append(p.Installs, "clickhouse-server "+c.ClickHouseVersion+".x")
		case n.Cfg.Has(config.RoleKeeper):
			p.Installs = append(p.Installs, "clickhouse-keeper "+c.ClickHouseVersion+".x (id "+fmt.Sprint(c.KeeperID(n.Cfg.Name))+")")
		}
		if n.Cfg.Has(config.RolePeerDB) {
			p.Installs = append(p.Installs, "peerdb "+peerdbVersion(c)+" + seaweedfs staging (docker compose)")
		}
		p.Warnings = n.Facts.Warnings(n.Cfg.Has(config.RoleClickHouse))
		plan = append(plan, p)
	}
	if asJSON {
		return printJSON(map[string]any{"cluster": c.Cluster, "hosts": plan, "warnings": warns})
	}
	fmt.Printf("cluster %s: %d clickhouse replica(s), %d keeper member(s)\n", c.Cluster,
		len(c.With(config.RoleClickHouse)), len(c.With(config.RoleKeeper)))
	for _, p := range plan {
		f := p.Facts
		if f.OSID == "" {
			fmt.Printf("\n%s  [%s]\n", p.Name, strings.Join(p.Roles, ","))
		} else {
			fmt.Printf("\n%s  [%s]  %s %s %s, %d CPU, %d GB RAM, %d GB free\n", p.Name, strings.Join(p.Roles, ","),
				f.OSID, f.OSVersion, f.Arch, f.CPUs, f.RAMBytes>>30, f.DiskFreeGB)
		}
		for _, i := range p.Installs {
			fmt.Println("  install:", i)
		}
		for _, w := range p.Warnings {
			fmt.Println("  warning:", w)
		}
	}
	for _, w := range warns {
		fmt.Println("warning:", w)
	}
	return nil
}

func cmdInstall(ctx context.Context, c *config.Config, nodes []*cluster.Node, asJSON bool) error {
	if err := cluster.Install(ctx, c, nodes, os.Stdout); err != nil {
		return err
	}
	if p := peerNode(nodes); p != nil {
		if err := peerdb.Install(ctx, c, p.Cfg, p.SSH, os.Stdout); err != nil {
			return err
		}
	}
	return cmdVerify(ctx, c, nodes, asJSON)
}

func peerNode(nodes []*cluster.Node) *cluster.Node {
	for _, n := range nodes {
		if n.Cfg.Has(config.RolePeerDB) {
			return n
		}
	}
	return nil
}

// peerdbChecks: the API answers, and ClickHouse can use the staging bucket.
func peerdbChecks(ctx context.Context, c *config.Config, nodes []*cluster.Node) []cluster.Check {
	p := peerNode(nodes)
	if p == nil {
		return nil
	}
	_, err := peerdb.Call(ctx, p.SSH, "GET", "/v1/dynamic_settings", nil)
	out := []cluster.Check{{Name: p.Cfg.Name + ": peerdb api answers", OK: err == nil, Info: errText(err),
		Fix: "cd to the peerdb dir on that host and run: docker compose -p chlift-peerdb ps"}}
	for _, n := range nodes {
		if n.Cfg.Has(config.RoleClickHouse) {
			err := peerdb.CheckStaging(ctx, c, p.Cfg, n.SSH)
			out = append(out, cluster.Check{Name: n.Cfg.Name + ": clickhouse reads/writes the peerdb staging bucket",
				OK: err == nil, Info: peerdb.Endpoint(c, p.Cfg) + " " + errText(err),
				Fix: "the staging endpoint must be reachable from every ClickHouse host (port 8333)"})
		}
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func cmdVerify(ctx context.Context, c *config.Config, nodes []*cluster.Node, asJSON bool) error {
	checks := append(cluster.Verify(ctx, c, nodes), peerdbChecks(ctx, c, nodes)...)
	failed := 0
	for _, ch := range checks {
		if !ch.OK {
			failed++
		}
	}
	if asJSON {
		if err := printJSON(map[string]any{"ok": failed == 0, "checks": checks}); err != nil {
			return err
		}
	} else {
		for _, ch := range checks {
			mark := "ok  "
			if !ch.OK {
				mark = "FAIL"
			}
			fmt.Printf("%s %s: %s\n", mark, ch.Name, ch.Info)
			if !ch.OK && ch.Fix != "" {
				fmt.Println("     fix:", ch.Fix)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func peerdbVersion(c *config.Config) string {
	if c.PeerDB.Version != "" {
		return c.PeerDB.Version
	}
	return peerdb.DefaultVersion
}

func cmdSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	dsnEnv := fs.String("dsn-env", "CHLIFT_PG_DSN", "env var holding the Postgres DSN")
	events := fs.Int("events", 4_000_000, "user_events rows (request_logs gets half, system_logs a quarter)")
	fs.Parse(args)
	dsn, err := config.Secret(*dsnEnv)
	if err != nil {
		return err
	}
	return migrate.Seed(ctx, dsn, *events, os.Stdout)
}

func cmdMigrate(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: chlift migrate <plan|start|status|check>")
	}
	sub := args[0]
	fs := flag.NewFlagSet("migrate "+sub, flag.ExitOnError)
	path := fs.String("config", "chlift.yaml", "config file")
	planPath := fs.String("plan", "chlift-migration.yaml", "migration plan file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	tables := fs.String("tables", "", "comma-separated schema.table list (plan); default: detect event tables")
	minRows := fs.Int64("min-rows", 100_000, "smallest table to propose (plan)")
	applyPG := fs.Bool("apply-postgres", false, "run the plan's Postgres changes (start)")
	runs := fs.Int("runs", 5, "runs per query (bench)")
	sums := fs.Bool("checksums", false, "also compare PK-range column checksums (check)")
	rawOut := fs.String("out", "bench-raw.json", "where bench writes every raw sample (bench)")
	mirror := fs.String("mirror", "", "mirror name (plan); default chlift_<cluster>")
	database := fs.String("database", "chlift", "ClickHouse database (plan)")
	listen := fs.String("listen", "127.0.0.1:9465", "metrics address (exporter)")
	checkEvery := fs.Duration("check-interval", 10*time.Minute, "how often the exporter runs the per-day check")
	fs.Parse(args[1:])
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	dsn, err := config.Secret(c.Postgres.DSNEnv)
	if err != nil {
		return err
	}
	if sub == "plan" {
		o := migrate.Options{MinRows: *minRows, Mirror: "chlift_" + c.Cluster, Database: *database}
		if *mirror != "" {
			o.Mirror = *mirror
		}
		if *tables != "" {
			o.Tables = strings.Split(*tables, ",")
		}
		p, err := migrate.Discover(ctx, dsn, o)
		if err != nil {
			return err
		}
		if err := migrate.Save(*planPath, p); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(p)
		}
		for _, t := range p.Tables {
			fmt.Printf("migrate %-28s %10d rows  ORDER BY (%s)  %s\n", t.Source, t.Rows, strings.Join(t.OrderBy, ", "), t.PartitionBy)
			for _, n := range t.Notes {
				fmt.Println("    note:", n)
			}
		}
		for _, s := range p.Skipped {
			fmt.Printf("keep    %-28s %s\n", s.Table, s.Reason)
		}
		for _, f := range p.PostgresFixes {
			fmt.Println("postgres:", f)
		}
		fmt.Printf("wrote %s: review it, then run `chlift migrate start`\n", *planPath)
		return nil
	}
	p, err := migrate.Load(*planPath)
	if err != nil {
		return err
	}
	nodes, err := cluster.Connect(ctx, c)
	if err != nil {
		return err
	}
	defer cluster.Close(nodes)
	pn := peerNode(nodes)
	if pn == nil {
		return fmt.Errorf("no peerdb host in %s", *path)
	}
	e := migrate.Env{Cfg: c, DSN: dsn, Peer: pn.SSH, Peers: pn.Cfg, Log: os.Stdout}
	for _, n := range nodes {
		if n.Cfg.Has(config.RoleClickHouse) {
			e.CH = append(e.CH, n)
		}
	}
	switch sub {
	case "start":
		return migrate.Start(ctx, e, p, *applyPG)
	case "abort":
		return migrate.Abort(ctx, e, p)
	case "cutover":
		return migrate.Cutover(ctx, e, p)
	case "exporter":
		x := &migrate.Exporter{Env: e, Plan: p, CheckInterval: *checkEvery}
		e.Log = io.Discard
		x.Env.Log = io.Discard
		fmt.Printf("chlift exporter for mirror %s on http://%s/metrics (per-day check every %s)\n", p.Mirror, *listen, *checkEvery)
		return x.Run(ctx, *listen)
	case "bench":
		res, err := migrate.Bench(ctx, dsn, e.CH[0], p.Database, *runs)
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(map[string]any{"runs": *runs, "taken_utc": time.Now().UTC().Format(time.RFC3339), "results": res}, "", "  ")
		if err := os.WriteFile(*rawOut, raw, 0o644); err != nil {
			return err
		}
		defer fmt.Printf("raw samples (every run, same invocation): %s\n", *rawOut)
		if *asJSON {
			return printJSON(res)
		}
		fmt.Printf("%-42s %12s %18s %14s\n", "query (demo dataset, median of runs)", "postgres", "clickhouse FINAL", "clickhouse")
		for _, r := range res {
			fmt.Printf("%-42s %10.0fms %16.0fms %12.0fms  (%.0fx / %.0fx)\n", r.Query, r.PGms, r.CHFinal, r.CHPlain, r.PGms/r.CHFinal, r.PGms/r.CHPlain)
		}
		return nil
	case "status":
		s, err := migrate.GetStatus(ctx, e, p)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(s)
		}
		fmt.Printf("mirror %s: %s, %d rows synced by CDC, replication slot holds %d MB of WAL\n", s.Mirror, s.State, s.RowsSynced, s.SlotBytes>>20)
		for _, t := range s.Tables {
			fmt.Printf("  %-22s postgres ~%d", t.Table, t.PGEstimate)
			for _, n := range e.CH {
				fmt.Printf("  %s %d", n.Cfg.Name, t.Replicas[n.Cfg.Name])
			}
			fmt.Println()
		}
		return nil
	case "check":
		res, err := migrate.Check(ctx, e, p)
		if err != nil {
			return err
		}
		failed := 0
		for _, r := range res {
			if !r.OK {
				failed++
			}
		}
		if *asJSON {
			if err := printJSON(map[string]any{"ok": failed == 0, "tables": res}); err != nil {
				return err
			}
		} else {
			for _, r := range res {
				mark := "ok  "
				if !r.OK {
					mark = "FAIL"
				}
				fmt.Printf("%s %s: postgres %d rows; replicas %v %s\n", mark, r.Table, r.PGRows, r.Replicas, r.Err)
				for i, d := range r.BadDays {
					if i == 10 {
						fmt.Printf("     ... %d more days differ\n", len(r.BadDays)-10)
						break
					}
					fmt.Println("     day", d)
				}
				for _, d := range r.Today {
					fmt.Println("     today (not judged, CDC may lag):", d)
				}
			}
		}
		if *sums {
			res, err := migrate.Checksums(ctx, e, p)
			if err != nil {
				return err
			}
			for tbl, bad := range res {
				for i, b := range bad {
					if i == 5 {
						fmt.Printf("     ... %d more buckets differ in %s\n", len(bad)-5, tbl)
						break
					}
					fmt.Println("     bucket", b)
					if !strings.HasPrefix(b, "skipped") {
						failed++
					}
				}
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d difference(s) found", failed)
		}
		return nil
	}
	return fmt.Errorf("unknown migrate command %q", sub)
}
