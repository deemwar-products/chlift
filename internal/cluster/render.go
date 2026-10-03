package cluster

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"text/template"

	"github.com/deemwar-products/chlift/internal/config"
)

//go:embed templates/*
var templates embed.FS

var tpl = template.Must(template.ParseFS(templates, "templates/*"))

type keeperPeer struct {
	ID   int
	Host string
}

type renderData struct {
	Cluster        string
	Host           config.Host
	ListenIP       string
	KeeperID       int // 0 = this host runs no Keeper
	Keepers        []keeperPeer
	Replicas       []string
	ClusterSecret  string
	PasswordSHA256 string
	MaxMemoryBytes int64
}

func newRenderData(c *config.Config, h config.Host, ramBytes int64, password, secret string) renderData {
	d := renderData{Cluster: c.Cluster, Host: h, ListenIP: h.ListenIP(), ClusterSecret: secret}
	if h.Has(config.RoleKeeper) {
		d.KeeperID = c.KeeperID(h.Name)
	}
	for _, k := range c.With(config.RoleKeeper) {
		d.Keepers = append(d.Keepers, keeperPeer{ID: c.KeeperID(k.Name), Host: k.Private()})
	}
	for _, r := range c.With(config.RoleClickHouse) {
		d.Replicas = append(d.Replicas, r.Private())
	}
	sum := sha256.Sum256([]byte(password))
	d.PasswordSHA256 = hex.EncodeToString(sum[:])
	// Leave a quarter of RAM for the OS, Keeper and page cache; ClickHouse's own default is 90%.
	d.MaxMemoryBytes = ramBytes * 3 / 4
	return d
}

func render(name string, d renderData) []byte {
	var b bytes.Buffer
	if err := tpl.ExecuteTemplate(&b, name, d); err != nil {
		panic(err) // templates are embedded and covered by tests
	}
	return b.Bytes()
}
