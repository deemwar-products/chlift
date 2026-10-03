// Package peerdb installs PeerDB with its SeaweedFS staging store on the peerdb host and talks to its API.
//
// PeerDB (AGPLv3) runs from its unmodified upstream images; chlift only writes the compose file around them.
package peerdb

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/template"
	"time"

	"github.com/deemwar-products/chlift/internal/config"
	"github.com/deemwar-products/chlift/internal/remote"
)

// DefaultVersion is the PeerDB release the replication soak validated.
const DefaultVersion = "stable-v0.37.10"

const (
	seaweedVersion = "latest"
	bucket         = "peerdb"
	project        = "chlift-peerdb"
)

//go:embed templates/*
var files embed.FS

var tpl = template.Must(template.ParseFS(files, "templates/compose.yml"))

type composeData struct {
	Version, SeaweedVersion, S3Endpoint, S3Bind, Network string
}

// Endpoint is the S3 staging URL as ClickHouse and PeerDB reach it.
func Endpoint(c *config.Config, h config.Host) string {
	if c.PeerDB.S3Endpoint != "" {
		return c.PeerDB.S3Endpoint
	}
	return "http://" + h.Private() + ":8333"
}

func version(c *config.Config) string {
	if c.PeerDB.Version != "" {
		return c.PeerDB.Version
	}
	return DefaultVersion
}

// Install converges the PeerDB stack on h. It needs docker with compose v2 for the SSH user.
func Install(ctx context.Context, c *config.Config, cfg config.Host, h *remote.Host, log io.Writer) error {
	if _, err := h.Run(ctx, "docker compose version >/dev/null"); err != nil {
		return fmt.Errorf("%s: docker with compose v2 is required on the peerdb host (install: curl -fsSL https://get.docker.com | sh): %w", cfg.Name, err)
	}
	s3Secret, err := config.Secret(config.EnvS3Secret)
	if err != nil {
		return err
	}
	catalogPW, err := config.Secret(config.EnvCatalogPassword)
	if err != nil {
		return err
	}
	dir, err := Dir(ctx, c, h)
	if err != nil {
		return err
	}
	var compose bytes.Buffer
	d := composeData{Version: version(c), SeaweedVersion: seaweedVersion, S3Endpoint: Endpoint(c, cfg),
		S3Bind: cfg.ListenIP(), Network: c.PeerDB.Network}
	if err := tpl.Execute(&compose, d); err != nil {
		return err
	}
	s3json, _ := json.Marshal(map[string]any{"identities": []any{map[string]any{
		"name": "chlift", "credentials": []any{map[string]string{"accessKey": "chlift", "secretKey": s3Secret}},
		"actions": []string{"Admin", "Read", "List", "Tagging", "Write"}}}})
	dyn, _ := files.ReadFile("templates/dynamicconfig.yaml")
	attr, _ := files.ReadFile("templates/search-attr.sh")
	writes := []struct {
		name string
		body []byte
		mode uint32
	}{
		{"compose.yml", compose.Bytes(), 0o644},
		{".env", []byte(fmt.Sprintf("%s=%s\n%s=%s\n", config.EnvCatalogPassword, catalogPW, config.EnvS3Secret, s3Secret)), 0o600},
		{"s3.json", s3json, 0o644}, // the seaweedfs image runs as non-root; the directory itself is 0700
		{"dynamicconfig.yaml", dyn, 0o644},
		{"search-attr.sh", attr, 0o755},
	}
	if _, err := h.Run(ctx, fmt.Sprintf("mkdir -p %q && chmod 700 %q", dir, dir)); err != nil {
		return err
	}
	s3Changed := false
	for _, w := range writes {
		ch, err := h.WriteFile(ctx, dir+"/"+w.name, w.body, modeOf(w.mode), "")
		if err != nil {
			return err
		}
		if ch {
			s3Changed = s3Changed || w.name == "s3.json"
			fmt.Fprintf(log, "%s: wrote %s/%s\n", cfg.Name, dir, w.name)
		}
	}
	if _, err := h.Run(ctx, fmt.Sprintf("cd %q && docker compose -p %s up -d --quiet-pull 2>&1 | tail -3", dir, project)); err != nil {
		return fmt.Errorf("%s: start peerdb: %w", cfg.Name, err)
	}
	// SeaweedFS reads its identities only at start (a rotated key needs a restart; .env changes recreate the rest).
	if s3Changed {
		if _, err := h.Run(ctx, fmt.Sprintf("cd %q && docker compose -p %s restart chlift-s3 >/dev/null 2>&1", dir, project)); err != nil {
			return err
		}
	}
	// compose can return with services left in "created"; insist every long-running service is up.
	if out, err := h.Run(ctx, fmt.Sprintf(`cd %q && docker compose -p %s ps -a --format '{{.Service}} {{.State}}' | grep -v '^flow-migrate ' | grep -v ' running$' || true`, dir, project)); err != nil {
		return err
	} else if out != "" {
		if _, err := h.Run(ctx, fmt.Sprintf("cd %q && docker compose -p %s up -d >/dev/null 2>&1", dir, project)); err != nil {
			return fmt.Errorf("%s: services not running (%s): %w", cfg.Name, strings.ReplaceAll(out, "\n", ", "), err)
		}
	}
	if err := waitAPI(ctx, h, 5*time.Minute); err != nil {
		return err
	}
	// Bucket creation is idempotent: an existing bucket is fine.
	if _, err := h.Run(ctx, fmt.Sprintf(`cd %q && docker compose -p %s exec -T chlift-s3 sh -c 'echo "s3.bucket.create -name %s" | weed shell' >/dev/null 2>&1 || true`, dir, project, bucket)); err != nil {
		return err
	}
	fmt.Fprintf(log, "%s: peerdb %s running (api on a localhost port of compose project %s), staging %s/%s\n", cfg.Name, version(c), project, Endpoint(c, cfg), bucket)
	return nil
}

// Dir is the compose directory on the peerdb host, with ~ expanded there.
func Dir(ctx context.Context, c *config.Config, h *remote.Host) (string, error) {
	dir := c.PeerDB.Dir
	if dir == "" {
		dir = "/opt/chlift/peerdb"
	}
	if strings.HasPrefix(dir, "~/") {
		home, err := h.Run(ctx, "echo $HOME")
		if err != nil {
			return "", err
		}
		dir = home + dir[1:]
	}
	return dir, nil
}

func waitAPI(ctx context.Context, h *remote.Host, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := Call(ctx, h, "GET", "/v1/dynamic_settings", nil); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("%s: peerdb api not ready after %s: %w", h.Name, timeout, err)
		}
		time.Sleep(5 * time.Second)
	}
}

// Call sends a request to the PeerDB API on the peerdb host. The body travels on stdin, never on a command line.
func Call(ctx context.Context, h *remote.Host, method, path string, body any) (json.RawMessage, error) {
	var in []byte
	if body != nil {
		var err error
		if in, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	// Address THIS project's flow-api by compose labels (exactly one container), never a fixed port:
	// another PeerDB on the same host once answered on the port chlift assumed.
	script := fmt.Sprintf(`id=$(docker ps -q --filter label=com.docker.compose.project=%s --filter label=com.docker.compose.service=flow-api)
[ "$(echo "$id" | grep -c .)" = 1 ] || { echo "expected 1 running flow-api in project %s, found: $id" >&2; exit 3; }
addr=$(docker port "$id" 8113/tcp | grep -m1 127.0.0.1)
[ -n "$addr" ] || { echo "flow-api has no published port" >&2; exit 3; }
curl -sS -X %s -H 'Content-Type: application/json' -w '\n%%{http_code}' "http://$addr%s"`, project, project, method, path)
	if in != nil {
		script += " --data-binary @-"
	}
	out, err := h.RunInput(ctx, script, in)
	if err != nil {
		return nil, err
	}
	i := strings.LastIndex(out, "\n")
	if i < 0 {
		return nil, fmt.Errorf("peerdb %s %s: no response", method, path)
	}
	respBody, code := out[:i], strings.TrimSpace(out[i+1:])
	if !strings.HasPrefix(code, "2") {
		return nil, fmt.Errorf("peerdb %s %s: HTTP %s: %s", method, path, code, respBody)
	}
	return json.RawMessage(respBody), nil
}

func modeOf(m uint32) fs.FileMode { return fs.FileMode(m) }

// CheckStaging proves ClickHouse itself can write and read the staging bucket (PeerDB never checks this: #4152).
// The query travels on stdin so the S3 secret never appears on a command line.
func CheckStaging(ctx context.Context, c *config.Config, cfg config.Host, ch *remote.Host) error {
	secret, err := config.Secret(config.EnvS3Secret)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/chlift-probe/%d.csv", Endpoint(c, cfg), bucket, time.Now().UnixNano())
	q := fmt.Sprintf("INSERT INTO FUNCTION s3('%[1]s', 'chlift', '%[2]s', 'CSV', 'x UInt8') SELECT 7;\n"+
		"SELECT sum(x) FROM s3('%[1]s', 'chlift', '%[2]s', 'CSV', 'x UInt8');\n", url, secret)
	out, err := ch.RunInput(ctx, "clickhouse-client --config-file=/etc/chlift/client.xml --multiquery", []byte(q))
	if err != nil {
		// clickhouse-client echoes the failing query, which carries the secret: never let it out.
		return errors.New(strings.ReplaceAll(err.Error(), secret, "[redacted]"))
	}
	if strings.TrimSpace(out) != "7" {
		return fmt.Errorf("wrote 7, read %q", out)
	}
	return nil
}
