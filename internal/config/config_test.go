package config

import (
	"strings"
	"testing"
)

func TestClickHouseVersionFormat(t *testing.T) {
	for v, ok := range map[string]bool{"26.3": true, "26.3.41.4": true, "26": false, "26.3.41": false, "26.3; rm -rf /": false, "latest": false} {
		c := &Config{Cluster: "x", ClickHouseVersion: v}
		errs, _ := c.Validate()
		bad := false
		for _, e := range errs {
			if strings.Contains(e, "clickhouse_version") {
				bad = true
			}
		}
		if bad == ok {
			t.Errorf("clickhouse_version %q: accepted=%v, want %v (errs %v)", v, !bad, ok, errs)
		}
	}
}
