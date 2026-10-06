package migrate

import (
	"strings"
	"testing"
)

// JSON must be compared (an emptied TOASTed value is the failure the checksums exist for), but by a
// form both stores agree on: whitespace stripped, because Postgres prints {"a": 1} and ClickHouse {"a":1}.
func TestSumColForJSON(t *testing.T) {
	for _, typ := range []string{"json", "jsonb"} {
		c, ok := sumColFor("properties", typ)
		if !ok {
			t.Fatalf("%s: not compared; an emptied TOAST value would go unnoticed", typ)
		}
		if !strings.Contains(c.pg, `regexp_replace("properties"::text, '\s', '', 'g')`) {
			t.Errorf("%s: postgres side does not strip whitespace: %s", typ, c.pg)
		}
		if !strings.Contains(c.ch, `replaceRegexpAll(toString(`+"`properties`"+`), '\\s', '')`) {
			t.Errorf("%s: clickhouse side does not strip whitespace: %s", typ, c.ch)
		}
	}
}

func TestSumColForTypes(t *testing.T) {
	for typ, want := range map[string]bool{
		"bigint": true, "boolean": true, "timestamp with time zone": true, "date": true, "text": true,
		"character varying(200)": true, "jsonb": true,
		"double precision": false, "real": false, "numeric(10,2)": false, "uuid": false,
	} {
		if _, ok := sumColFor("c", typ); ok != want {
			t.Errorf("%s: compared=%v, want %v", typ, ok, want)
		}
	}
}
