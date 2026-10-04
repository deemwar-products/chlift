package cluster

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/deemwar-products/chlift/internal/config"
	"github.com/deemwar-products/chlift/internal/remote"
)

// Node is one connected host with its facts.
type Node struct {
	Cfg   config.Host
	SSH   *remote.Host
	Facts Facts
}

func (n *Node) clickhouse() bool { return n.Cfg.Has(config.RoleClickHouse) }

// keeperOnly hosts run the standalone clickhouse-keeper; keepers on ClickHouse hosts are embedded.
func (n *Node) keeperOnly() bool { return n.Cfg.Has(config.RoleKeeper) && !n.clickhouse() }

// Connect dials and probes every host. The caller closes the nodes.
func Connect(ctx context.Context, c *config.Config) ([]*Node, error) {
	var nodes []*Node
	for _, h := range c.Hosts {
		s, err := remote.Dial(c, h)
		if err != nil {
			Close(nodes)
			return nil, err
		}
		n := &Node{Cfg: h, SSH: s}
		nodes = append(nodes, n)
		if n.clickhouse() || n.Cfg.Has(config.RoleKeeper) {
			if n.Facts, err = Probe(ctx, s); err != nil {
				Close(nodes)
				return nil, err
			}
		}
	}
	return nodes, nil
}

func Close(nodes []*Node) {
	for _, n := range nodes {
		n.SSH.Close()
	}
}

// Install converges every host to the config. Re-running it changes nothing unless the config changed.
func Install(ctx context.Context, c *config.Config, nodes []*Node, log io.Writer) error {
	password, err := config.Secret(config.EnvCHPassword)
	if err != nil {
		return err
	}
	secret, err := config.Secret(config.EnvClusterSecret)
	if err != nil {
		return err
	}
	build, err := pickBuild(ctx, c, nodes)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "ClickHouse build for every host: %s\n", build)
	changed := map[string]bool{}
	for _, n := range nodes {
		if !n.clickhouse() && !n.keeperOnly() {
			continue
		}
		v, pkgChanged, err := installPackages(ctx, n, build)
		if err != nil {
			return err
		}
		if pkgChanged {
			changed[n.Cfg.Name] = true
			fmt.Fprintf(log, "%s: ClickHouse %s installed\n", n.Cfg.Name, v)
		}
		d := newRenderData(c, n.Cfg, n.Facts.RAMBytes, password, secret)
		files := map[string][]byte{}
		if n.clickhouse() {
			files["/etc/clickhouse-server/config.d/chlift.xml"] = render("server.xml", d)
			files["/etc/clickhouse-server/users.d/chlift.xml"] = render("users.xml", d)
		} else {
			files["/etc/clickhouse-keeper/keeper_config.xml"] = render("keeper.xml", d)
		}
		files["/etc/chlift/client.xml"] = clientConfig(password)
		for path, body := range files {
			mode, owner := uint32(0o640), "root:clickhouse"
			if strings.HasPrefix(path, "/etc/chlift/") {
				mode, owner = 0o600, "root:root"
			}
			ch, err := n.SSH.WriteFile(ctx, path, body, osMode(mode), owner)
			if err != nil {
				return err
			}
			if ch {
				changed[n.Cfg.Name] = true
				fmt.Fprintf(log, "%s: wrote %s\n", n.Cfg.Name, path)
			}
		}
	}
	// Start or restart services: Keeper-only hosts first, so ClickHouse finds a quorum.
	for _, pass := range []bool{true, false} {
		for _, n := range nodes {
			if n.keeperOnly() != pass || (!n.clickhouse() && !n.keeperOnly()) {
				continue
			}
			if err := ensureService(ctx, n, changed[n.Cfg.Name], log); err != nil {
				return err
			}
			// Rolling: a restarted replica must answer before the next one goes down.
			if n.clickhouse() {
				if err := waitSQL(ctx, n, 120*time.Second); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// pickBuild decides the one ClickHouse build every host runs. A mixed cluster is never created:
// an explicit clickhouse_build wins; otherwise the build already installed is kept (re-runs never upgrade);
// otherwise the newest build in the line is resolved once, on the first host.
func pickBuild(ctx context.Context, c *config.Config, nodes []*Node) (string, error) {
	if c.ClickHouseBuild != "" {
		if !strings.HasPrefix(c.ClickHouseBuild, c.ClickHouseVersion+".") {
			return "", fmt.Errorf("clickhouse_build %s is not in line %s", c.ClickHouseBuild, c.ClickHouseVersion)
		}
		return c.ClickHouseBuild, nil
	}
	seen := map[string][]string{}
	var first *Node
	for _, n := range nodes {
		if !n.clickhouse() && !n.keeperOnly() {
			continue
		}
		if first == nil {
			first = n
		}
		if v := n.Facts.CHVersion; v != "" {
			seen[v] = append(seen[v], n.Cfg.Name)
		}
	}
	if len(seen) > 1 {
		var parts []string
		for v, hs := range seen {
			parts = append(parts, v+" on "+strings.Join(hs, ","))
		}
		return "", fmt.Errorf("hosts run different ClickHouse builds (%s); set clickhouse_build in the config to the one you want", strings.Join(parts, "; "))
	}
	for v := range seen {
		if strings.HasPrefix(v, c.ClickHouseVersion+".") {
			return v, nil
		}
	}
	return resolveBuild(ctx, first, c.ClickHouseVersion)
}

func resolveBuild(ctx context.Context, n *Node, line string) (string, error) {
	var script string
	switch n.Facts.Family() {
	case "deb":
		script = fmt.Sprintf(`
export DEBIAN_FRONTEND=noninteractive
if [ ! -f /usr/share/keyrings/clickhouse-keyring.gpg ]; then
  apt-get update -qq && apt-get install -y -qq apt-transport-https ca-certificates curl gnupg >/dev/null
  curl -fsSL https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key | gpg --dearmor -o /usr/share/keyrings/clickhouse-keyring.gpg
  echo "deb [signed-by=/usr/share/keyrings/clickhouse-keyring.gpg arch=$(dpkg --print-architecture)] https://packages.clickhouse.com/deb lts main" > /etc/apt/sources.list.d/clickhouse.list
fi
apt-get update -qq
apt-cache madison clickhouse-common-static | awk '{print $3}' | grep '^%[1]s\.' | sort -V | tail -1`, line)
	case "rpm":
		script = fmt.Sprintf(`
if [ ! -f /etc/yum.repos.d/clickhouse.repo ]; then
  (dnf install -y -q dnf-plugins-core || yum install -y -q yum-utils) >/dev/null
  (dnf config-manager --add-repo https://packages.clickhouse.com/rpm/clickhouse.repo || yum-config-manager --add-repo https://packages.clickhouse.com/rpm/clickhouse.repo) >/dev/null
fi
# Import the repo key up front: without it the first yum call asks to import it, gets no answer, and fails.
rpm -q gpg-pubkey --qf '%%{SUMMARY}\n' | grep -qi clickhouse || rpm --import https://packages.clickhouse.com/rpm/stable/repodata/repomd.xml.key
yum -y --showduplicates list clickhouse-common-static 2>/dev/null | awk '{print $2}' | grep '^%[1]s\.' | sort -V | tail -1`, line)
	}
	v, err := n.SSH.Run(ctx, script)
	if err == nil && v == "" {
		err = fmt.Errorf("no %s build in the ClickHouse repo", line)
	}
	if err != nil {
		return "", fmt.Errorf("%s: resolve ClickHouse build: %w", n.Cfg.Name, err)
	}
	return v, nil
}

// installPackages installs the exact build and reports whether the packages changed (which forces a restart).
func installPackages(ctx context.Context, n *Node, build string) (string, bool, error) {
	pkgs := "clickhouse-common-static=$V clickhouse-server=$V clickhouse-client=$V"
	probePkg := "clickhouse-server"
	if n.keeperOnly() {
		pkgs, probePkg = "clickhouse-keeper=$V", "clickhouse-keeper"
	}
	var script string
	switch n.Facts.Family() {
	case "deb":
		script = fmt.Sprintf(`
export DEBIAN_FRONTEND=noninteractive
if [ ! -f /usr/share/keyrings/clickhouse-keyring.gpg ]; then
  apt-get update -qq && apt-get install -y -qq apt-transport-https ca-certificates curl gnupg >/dev/null
  curl -fsSL https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key | gpg --dearmor -o /usr/share/keyrings/clickhouse-keyring.gpg
  echo "deb [signed-by=/usr/share/keyrings/clickhouse-keyring.gpg arch=$(dpkg --print-architecture)] https://packages.clickhouse.com/deb lts main" > /etc/apt/sources.list.d/clickhouse.list
  apt-get update -qq
fi
V=%[2]s
C=same
if [ "$(dpkg-query -W -f='${Version}' %[1]s 2>/dev/null || true)" != "$V" ]; then
  C=changed
  apt-get install -y -qq --allow-downgrades -o Dpkg::Options::=--force-confold %[3]s >/dev/null || \
    { apt-get update -qq; apt-get install -y -qq --allow-downgrades -o Dpkg::Options::=--force-confold %[3]s >/dev/null; }
fi
id clickhouse >/dev/null 2>&1 || useradd -r -U -s /bin/false clickhouse
echo "$V $C"`, probePkg, build, pkgs)
	case "rpm":
		// Written to the ClickHouse docs; not yet exercised in CI.
		script = fmt.Sprintf(`
if [ ! -f /etc/yum.repos.d/clickhouse.repo ]; then
  (dnf install -y -q dnf-plugins-core || yum install -y -q yum-utils) >/dev/null
  (dnf config-manager --add-repo https://packages.clickhouse.com/rpm/clickhouse.repo || yum-config-manager --add-repo https://packages.clickhouse.com/rpm/clickhouse.repo) >/dev/null
fi
# Import the repo key up front: without it the first yum call asks to import it, gets no answer, and fails.
rpm -q gpg-pubkey --qf '%%{SUMMARY}\n' | grep -qi clickhouse || rpm --import https://packages.clickhouse.com/rpm/stable/repodata/repomd.xml.key
V=%[2]s
C=same
if [ "$(rpm -q --qf '%%{VERSION}' %[1]s 2>/dev/null || true)" != "$V" ]; then
  C=changed
  yum install -y -q %[3]s >/dev/null
fi
id clickhouse >/dev/null 2>&1 || useradd -r -U -s /bin/false clickhouse
echo "$V $C"`, probePkg, build, strings.ReplaceAll(pkgs, "=$V", "-$V"))
	}
	out, err := n.SSH.Run(ctx, script)
	if err != nil {
		return "", false, fmt.Errorf("%s: install packages: %w", n.Cfg.Name, err)
	}
	lines := strings.Split(out, "\n")
	v, state, _ := strings.Cut(lines[len(lines)-1], " ")
	return v, state == "changed", nil
}

// ensureService starts the service, restarting it when its config changed.
// systemd hosts use the packaged units; hosts without systemd (containers, minimal images)
// run the daemon directly with a pid file (supervisor mode).
func ensureService(ctx context.Context, n *Node, restart bool, log io.Writer) error {
	unit, bin, conf := "clickhouse-server", "clickhouse-server", "/etc/clickhouse-server/config.xml"
	if n.keeperOnly() {
		unit, bin, conf = "clickhouse-keeper", "clickhouse-keeper", "/etc/clickhouse-keeper/keeper_config.xml"
	}
	var script string
	if n.Facts.Systemd {
		verb := "start"
		if restart {
			verb = "restart"
		}
		script = fmt.Sprintf("systemctl enable -q %[1]s && systemctl %[2]s %[1]s", unit, verb)
	} else {
		pid := fmt.Sprintf("/var/run/%[1]s/%[1]s.pid", unit)
		script = fmt.Sprintf(`
mkdir -p /var/run/%[1]s /var/lib/%[1]s /var/log/%[1]s && chown clickhouse:clickhouse /var/run/%[1]s /var/lib/%[1]s /var/log/%[1]s
running() { [ -f %[2]s ] && kill -0 "$(cat %[2]s)" 2>/dev/null; }
if running && [ "%[5]t" = true ]; then
  kill "$(cat %[2]s)"; i=0; while running && [ $i -lt 120 ]; do sleep 1; i=$((i+1)); done
  running && { echo "%[1]s did not stop" >&2; exit 1; }
fi
running || su -s /bin/sh clickhouse -c "%[3]s --daemon --config-file=%[4]s --pid-file=%[2]s"`, unit, pid, bin, conf, restart)
	}
	if _, err := n.SSH.Run(ctx, script); err != nil {
		return fmt.Errorf("%s: start %s: %w", n.Cfg.Name, unit, err)
	}
	action := "running"
	if restart {
		action = "restarted"
	}
	fmt.Fprintf(log, "%s: %s %s\n", n.Cfg.Name, unit, action)
	return nil
}

// SQL runs a query on a ClickHouse host with the client config chlift installed (no password on the command line).
func SQL(ctx context.Context, n *Node, q string) (string, error) {
	return n.SSH.Run(ctx, "clickhouse-client --config-file=/etc/chlift/client.xml -q "+shellQuote(q))
}

func waitSQL(ctx context.Context, n *Node, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := SQL(ctx, n, "SELECT 1")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: ClickHouse not answering after %s: %w", n.Cfg.Name, timeout, err)
		}
		time.Sleep(3 * time.Second)
	}
}

func clientConfig(password string) []byte {
	return []byte(fmt.Sprintf("<config>\n  <user>default</user>\n  <password>%s</password>\n</config>\n", xmlEscape(password)))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
