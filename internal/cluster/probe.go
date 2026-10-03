// Package cluster plans, installs and verifies a replicated ClickHouse cluster over SSH.
package cluster

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/deemwar-products/chlift/internal/remote"
)

// Facts is what chlift learns about a host before touching it.
type Facts struct {
	OSID       string // debian, ubuntu, rhel, rocky, almalinux, ...
	OSVersion  string
	Arch       string
	CPUs       int
	RAMBytes   int64 // cgroup limit when lower than physical RAM (containers)
	DiskFreeGB int64 // on /var/lib
	Systemd    bool  // PID 1 is systemd
	CHVersion  string
}

func (f Facts) Family() string {
	switch f.OSID {
	case "debian", "ubuntu":
		return "deb"
	case "rhel", "centos", "rocky", "almalinux", "fedora", "ol", "amzn":
		return "rpm"
	}
	return ""
}

const probeScript = `
. /etc/os-release; echo "os=$ID"; echo "ver=$VERSION_ID"
echo "arch=$(uname -m)"
echo "cpus=$(nproc)"
echo "ram=$(awk '/MemTotal/{print $2*1024}' /proc/meminfo)"
lim=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || echo max)
echo "cgmem=$lim"
mkdir -p /var/lib; echo "disk=$(df -Pk /var/lib | awk 'NR==2{print int($4/1048576)}')"
echo "init=$(cat /proc/1/comm)"
echo "ch=$( (clickhouse --version || clickhouse-keeper --version) 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
`

func Probe(ctx context.Context, h *remote.Host) (Facts, error) {
	out, err := h.Run(ctx, probeScript)
	if err != nil {
		return Facts{}, err
	}
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = strings.Trim(v, `"`)
		}
	}
	f := Facts{OSID: kv["os"], OSVersion: kv["ver"], Arch: kv["arch"], Systemd: kv["init"] == "systemd", CHVersion: kv["ch"]}
	f.CPUs, _ = strconv.Atoi(kv["cpus"])
	f.RAMBytes, _ = strconv.ParseInt(kv["ram"], 10, 64)
	if lim, err := strconv.ParseInt(kv["cgmem"], 10, 64); err == nil && lim > 0 && lim < f.RAMBytes {
		f.RAMBytes = lim
	}
	f.DiskFreeGB, _ = strconv.ParseInt(kv["disk"], 10, 64)
	if f.Family() == "" {
		return f, fmt.Errorf("%s: unsupported OS %q (Debian/Ubuntu and RHEL-family only)", h.Name, f.OSID)
	}
	return f, nil
}

// Warnings are the plan-time hardware concerns, from ClickHouse's own sizing guidance.
func (f Facts) Warnings(clickhouse bool) []string {
	var w []string
	gb := f.RAMBytes >> 30
	if clickhouse && gb < 16 {
		w = append(w, fmt.Sprintf("%d GB RAM: ClickHouse docs advise 16 GB minimum, 32 GB+ for production; fine for dev/test", gb))
	}
	if f.DiskFreeGB < 20 {
		w = append(w, fmt.Sprintf("only %d GB free on /var/lib", f.DiskFreeGB))
	}
	if !f.Systemd {
		w = append(w, "no systemd: services run under chlift's supervisor mode (dev containers, minimal images)")
	}
	return w
}
