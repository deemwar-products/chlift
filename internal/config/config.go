// Package config reads and writes chlift.yaml and the local secrets file.
//
// chlift.yaml never holds a password: secrets live in environment variables, or in a local
// secrets file (mode 0600) that chlift loads into the environment when a variable is unset.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// chVersionRe accepts an LTS line (26.3: the newest build of that line at install) or an exact build (26.3.41.4).
var chVersionRe = regexp.MustCompile(`^\d+\.\d+(\.\d+\.\d+)?$`)

const (
	RoleClickHouse = "clickhouse"
	RoleKeeper     = "keeper"
	RolePeerDB     = "peerdb"

	EnvCHPassword      = "CHLIFT_CH_PASSWORD"
	EnvClusterSecret   = "CHLIFT_CLUSTER_SECRET"
	EnvS3Secret        = "CHLIFT_S3_SECRET"
	EnvCatalogPassword = "CHLIFT_PEERDB_CATALOG_PASSWORD"
)

type Config struct {
	Version           int      `yaml:"version"`
	Cluster           string   `yaml:"cluster"`
	ClickHouseVersion string   `yaml:"clickhouse_version"`         // LTS line, e.g. "26.3"
	ClickHouseBuild   string   `yaml:"clickhouse_build,omitempty"` // exact build every host runs; empty = keep what is installed, else newest in the line
	SSH               SSH      `yaml:"ssh"`
	Hosts             []Host   `yaml:"hosts"`
	Postgres          Postgres `yaml:"postgres"`
	PeerDB            PeerDB   `yaml:"peerdb,omitempty"`
	SecretsFile       string   `yaml:"secrets_file,omitempty"`
}

type SSH struct {
	User                  string `yaml:"user"`
	Port                  int    `yaml:"port"`
	KeyFile               string `yaml:"key_file,omitempty"` // empty = ssh-agent
	KnownHosts            string `yaml:"known_hosts,omitempty"`
	InsecureIgnoreHostKey bool   `yaml:"insecure_ignore_host_key,omitempty"` // dev containers only
}

type Host struct {
	Name           string   `yaml:"name"`
	Address        string   `yaml:"address"`                   // how chlift reaches it over SSH
	Port           int      `yaml:"port,omitempty"`            // SSH port override
	PrivateAddress string   `yaml:"private_address,omitempty"` // how cluster members reach each other; default Address
	Roles          []string `yaml:"roles"`
}

type Postgres struct {
	DSNEnv string `yaml:"dsn_env"` // name of the env var holding the source DSN (as chlift reaches it)
	// How PeerDB reaches Postgres, when that differs from the DSN (tunnels, docker networks).
	PeerHost string `yaml:"peer_host,omitempty"`
	PeerPort int    `yaml:"peer_port,omitempty"`
}

type PeerDB struct {
	Version    string `yaml:"version,omitempty"`     // PeerDB image tag; default the soak-tested release
	Dir        string `yaml:"dir,omitempty"`         // compose dir on the peerdb host
	Network    string `yaml:"network,omitempty"`     // join this existing docker network (dev environments)
	S3Endpoint string `yaml:"s3_endpoint,omitempty"` // staging store URL as ClickHouse AND PeerDB reach it; default http://<peerdb private>:8333
}

func (h Host) Has(role string) bool { return slices.Contains(h.Roles, role) }

func (h Host) Private() string {
	if h.PrivateAddress != "" {
		return h.PrivateAddress
	}
	return h.Address
}

func (h Host) SSHPort(c *Config) int {
	if h.Port != 0 {
		return h.Port
	}
	if c.SSH.Port != 0 {
		return c.SSH.Port
	}
	return 22
}

// ListenIP is the address ClickHouse and Keeper bind to: the private IP when one is configured,
// else all interfaces (plan warns about that).
func (h Host) ListenIP() string {
	if ip := net.ParseIP(h.Private()); ip != nil {
		return ip.String()
	}
	return "0.0.0.0"
}

func (c *Config) With(role string) []Host {
	var out []Host
	for _, h := range c.Hosts {
		if h.Has(role) {
			out = append(out, h)
		}
	}
	return out
}

// KeeperID is the stable Raft server id of a keeper host (1-based, in file order).
func (c *Config) KeeperID(name string) int {
	for i, h := range c.With(RoleKeeper) {
		if h.Name == name {
			return i + 1
		}
	}
	return 0
}

// Validate returns hard errors (install refuses) and warnings (plan prints).
func (c *Config) Validate() (errs, warns []string) {
	if c.Cluster == "" {
		errs = append(errs, "cluster name is empty")
	}
	if c.ClickHouseVersion == "" {
		errs = append(errs, "clickhouse_version is empty (use an LTS line such as 26.3)")
	} else if !chVersionRe.MatchString(c.ClickHouseVersion) {
		errs = append(errs, fmt.Sprintf("clickhouse_version %q: use an LTS line such as 26.3, or an exact build such as 26.3.41.4 to pin it", c.ClickHouseVersion))
	}
	names := map[string]bool{}
	for _, h := range c.Hosts {
		if names[h.Name] {
			errs = append(errs, "duplicate host name "+h.Name)
		}
		names[h.Name] = true
		if h.Address == "" {
			errs = append(errs, h.Name+": address is empty")
		}
		if h.ListenIP() == "0.0.0.0" {
			warns = append(warns, h.Name+": private_address is not an IP, so services bind to all interfaces; firewall them")
		}
	}
	chs, ks := len(c.With(RoleClickHouse)), len(c.With(RoleKeeper))
	switch {
	case chs == 0:
		errs = append(errs, "no clickhouse hosts")
	case chs == 1:
		warns = append(warns, "one clickhouse host: no replica, a host loss is an outage")
	}
	switch {
	case ks == 2:
		// Two Raft members need both for quorum: losing either makes every replicated table read-only.
		errs = append(errs, "2 keeper members can't survive a failure (both are needed for quorum); use 3 (add a small third host) or 1 for a single-node test")
	case ks%2 == 0 && ks > 0:
		errs = append(errs, fmt.Sprintf("%d keeper members: use an odd number", ks))
	case ks == 0:
		errs = append(errs, "no keeper hosts")
	case ks == 1 && chs > 1:
		warns = append(warns, "one keeper member: losing it makes replicated tables read-only")
	}
	if len(c.With(RolePeerDB)) > 1 {
		errs = append(errs, "at most one peerdb host")
	}
	return errs, warns
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.SSH.User == "" {
		c.SSH.User = "root"
	}
	if c.SecretsFile == "" {
		c.SecretsFile = DefaultSecretsFile(c.Cluster)
	}
	return &c, loadSecrets(c.SecretsFile)
}

func Save(path string, c *Config) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func DefaultSecretsFile(cluster string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "chlift", cluster+".env")
}

// EnsureSecrets generates any missing secret into the secrets file. Values are never printed.
func EnsureSecrets(path string) (created []string, err error) {
	if err := loadSecrets(path); err != nil {
		return nil, err
	}
	var add []string
	for _, k := range []string{EnvCHPassword, EnvClusterSecret, EnvS3Secret, EnvCatalogPassword} {
		if os.Getenv(k) == "" {
			b := make([]byte, 24)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}
			v := hex.EncodeToString(b)
			os.Setenv(k, v)
			add = append(add, k+"="+v)
			created = append(created, k)
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(add, "\n") + "\n")
	return created, err
}

// Secret returns a required secret from the environment (after the secrets file is loaded).
func Secret(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is not set (run `chlift init` to generate it, or export it)", name)
}

// loadSecrets sets KEY=VALUE lines from the file for keys not already in the environment.
func loadSecrets(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(s.Text()), "=")
		if ok && !strings.HasPrefix(k, "#") && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return s.Err()
}
