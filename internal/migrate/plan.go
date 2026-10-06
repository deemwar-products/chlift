// Package migrate plans, starts and checks the Postgres -> ClickHouse migration of event tables.
package migrate

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

// Plan is chlift-migration.yaml: reviewed by a human, then executed by `migrate start`.
type Plan struct {
	Mirror        string    `yaml:"mirror"`
	Database      string    `yaml:"database"` // ClickHouse database (Replicated engine)
	Publication   string    `yaml:"publication"`
	Tables        []Table   `yaml:"tables"`
	Skipped       []Skipped `yaml:"skipped,omitempty"`
	PostgresFixes []string  `yaml:"postgres_fixes,omitempty"` // run by `migrate start --apply-postgres`, or by hand
}

type Table struct {
	Source      string   `yaml:"source"` // schema.table
	Target      string   `yaml:"target"`
	Rows        int64    `yaml:"rows"`
	SizeBytes   int64    `yaml:"size_bytes"`
	PrimaryKey  []string `yaml:"primary_key"`
	TimeColumn  string   `yaml:"time_column,omitempty"`
	TimeType    string   `yaml:"time_type,omitempty"`
	OrderBy     []string `yaml:"order_by"`
	PartitionBy string   `yaml:"partition_by,omitempty"`
	Mutated     bool     `yaml:"mutated"` // sees UPDATE/DELETE
	Notes       []string `yaml:"notes,omitempty"`
}

type Skipped struct {
	Table  string `yaml:"table"`
	Reason string `yaml:"reason"`
}

type column struct{ name, typ, storage string }

type pgTable struct {
	schema, name              string
	rows, size, ins, upd, del int64
	replIdent                 string
	pk                        []string
	cols                      []column
}

const discoverSQL = `
SELECT n.nspname, c.relname, greatest(c.reltuples,0)::bigint, pg_total_relation_size(c.oid),
       coalesce(s.n_tup_ins,0), coalesce(s.n_tup_upd,0), coalesce(s.n_tup_del,0), c.relreplident::text,
       coalesce((SELECT array_agg(a.attname::text ORDER BY array_position(i.indkey::int[], a.attnum::int))
                 FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
                 WHERE i.indrelid = c.oid AND i.indisprimary), '{}'),
       (SELECT array_agg(a.attname || '|' || format_type(a.atttypid, a.atttypmod) || '|' || t.typstorage::text ORDER BY a.attnum)
          FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
         WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r','p') AND NOT c.relispartition
  AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
  AND n.nspname NOT LIKE '_peerdb%'
ORDER BY pg_total_relation_size(c.oid) DESC`

// Options steer the proposal.
type Options struct {
	Tables   []string // explicit schema.table list; empty = auto-detect event tables
	MinRows  int64
	Mirror   string
	Database string
}

var timeNames = []string{"event_time", "occurred_at", "created_at", "timestamp", "ts", "time", "inserted_at", "logged_at"}
var tenantNames = []string{"tenant_id", "account_id", "org_id", "organization_id", "workspace_id", "project_id", "customer_id", "user_id"}

// Discover reads the source schema and proposes a plan. It changes nothing.
func Discover(ctx context.Context, dsn string, o Options) (*Plan, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, discoverSQL)
	if err != nil {
		return nil, err
	}
	var all []pgTable
	for rows.Next() {
		var t pgTable
		var cols []string
		if err := rows.Scan(&t.schema, &t.name, &t.rows, &t.size, &t.ins, &t.upd, &t.del, &t.replIdent, &t.pk, &cols); err != nil {
			return nil, err
		}
		for _, c := range cols {
			p := strings.SplitN(c, "|", 3)
			t.cols = append(t.cols, column{p[0], p[1], p[2]})
		}
		all = append(all, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if o.Mirror == "" {
		o.Mirror = "chlift_events"
	}
	if o.Database == "" {
		o.Database = "chlift"
	}
	p := &Plan{Mirror: o.Mirror, Database: o.Database, Publication: o.Mirror + "_pub"}
	var pubTables []string
	for _, t := range all {
		full := t.schema + "." + t.name
		explicit := slices.Contains(o.Tables, full)
		if len(o.Tables) > 0 && !explicit {
			continue
		}
		tm, reason := propose(t, o.MinRows, explicit)
		if reason != "" {
			p.Skipped = append(p.Skipped, Skipped{full, reason})
			continue
		}
		if why := needsFullIdentity(t, tm); why != "" && t.replIdent != "f" {
			p.PostgresFixes = append(p.PostgresFixes, fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY FULL", quoteIdent(t.schema, t.name)))
			tm.Notes = append(tm.Notes, "needs REPLICA IDENTITY FULL: "+why)
		}
		p.Tables = append(p.Tables, tm)
		pubTables = append(pubTables, quoteIdent(t.schema, t.name))
	}
	if len(pubTables) > 0 {
		p.PostgresFixes = append(p.PostgresFixes, fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s", p.Publication, strings.Join(pubTables, ", ")))
	}
	return p, nil
}

func propose(t pgTable, minRows int64, explicit bool) (Table, string) {
	full := t.schema + "." + t.name
	if len(t.pk) == 0 {
		return Table{}, "no primary key (PeerDB needs one to apply updates and deletes)"
	}
	timeCol := pickTime(t.cols)
	mutated := t.upd+t.del > 0
	if !explicit {
		switch {
		case timeCol == "" && rewrittenTime(t.cols) != "":
			return Table{}, "only " + rewrittenTime(t.cols) + ", which updates rewrite: not an event table (name it with --tables to migrate anyway; it then sorts by primary key)"
		case timeCol == "":
			return Table{}, "no timestamp column: not an event table (name it with --tables to migrate anyway)"
		case t.rows < minRows:
			return Table{}, fmt.Sprintf("%d rows < --min-rows %d", t.rows, minRows)
		case t.ins > 0 && float64(t.upd+t.del) > 0.05*float64(t.ins):
			return Table{}, fmt.Sprintf("entity-like: %d updates+deletes vs %d inserts; keep it in Postgres", t.upd+t.del, t.ins)
		}
	}
	tm := Table{Source: full, Target: t.name, Rows: t.rows, SizeBytes: t.size, PrimaryKey: t.pk, TimeColumn: timeCol, Mutated: mutated}
	for _, c := range t.cols {
		if c.name == timeCol {
			tm.TimeType = c.typ
		}
	}
	// ReplacingMergeTree de-duplicates on the sorting key, so the key must contain the primary key.
	// A time-first key is only safe when the leading columns never change for a row, so mutated tables sort by PK.
	if !mutated && timeCol != "" {
		if tenant := pick(t.cols, tenantNames, func(string) bool { return true }); tenant != "" && !slices.Contains(t.pk, tenant) {
			tm.OrderBy = append(tm.OrderBy, tenant)
			tm.Notes = append(tm.Notes, "sorted by "+tenant+" first: an UPDATE that changes a row's "+tenant+
				" leaves the old row live in ClickHouse (migrate check reports it); don't migrate tables whose rows move between "+tenant+"s")
		}
		if !slices.Contains(t.pk, timeCol) {
			tm.OrderBy = append(tm.OrderBy, timeCol)
		}
	} else if mutated {
		tm.Notes = append(tm.Notes, "updated or deleted rows: sorted by primary key so de-duplication stays correct; queries need FINAL")
	}
	tm.OrderBy = append(tm.OrderBy, t.pk...)
	if timeCol != "" && !mutated {
		tm.PartitionBy = "toYYYYMM(" + timeCol + ")"
	}
	tm.Notes = append(tm.Notes, "sort key and partitioning cannot change after the table is created (PeerDB #4604): review them now")
	return tm, ""
}

func pick(cols []column, names []string, ok func(typ string) bool) string {
	for _, n := range names {
		for _, c := range cols {
			if c.name == n && ok(c.typ) {
				return c.name
			}
		}
	}
	return ""
}

// pickTime prefers well-known event-time names, else the first timestamp column that is not rewritten by updates.
// A column such as updated_at must never lead the sort key: ReplacingMergeTree folds rows by the whole key, so an
// UPDATE that changes it leaves the old row live in ClickHouse. The mutated check can't be relied on for this
// (Postgres's update counters are zero on a fresh table, after a stats reset or a crash, and on a new replica).
func pickTime(cols []column) string {
	if n := pick(cols, timeNames, isTime); n != "" {
		return n
	}
	for _, c := range cols {
		if isTime(c.typ) && !rewrittenOnUpdate(c.name) {
			return c.name
		}
	}
	return ""
}

var rewrittenRe = regexp.MustCompile(`(^|_)(updated|modified|changed|edited|synced|touched|refreshed|deleted)(_|$)|^last_|_last$`)

// rewrittenTime returns the first timestamp column that updates rewrite, or "".
func rewrittenTime(cols []column) string {
	for _, c := range cols {
		if isTime(c.typ) && rewrittenOnUpdate(c.name) {
			return c.name
		}
	}
	return ""
}

// rewrittenOnUpdate says a timestamp column's name marks it as changed by updates (updated_at, last_seen_at, ...).
func rewrittenOnUpdate(name string) bool { return rewrittenRe.MatchString(strings.ToLower(name)) }

func isTime(typ string) bool {
	return strings.HasPrefix(typ, "timestamp") || typ == "date"
}

// needsFullIdentity says why a table must ship full old rows in the WAL, or "" if it need not.
// Both reasons were found as silent corruption in chlift's own tests, never as errors.
func needsFullIdentity(t pgTable, tm Table) string {
	if len(tm.OrderBy) > 0 && len(t.pk) > 0 && tm.OrderBy[0] != t.pk[0] {
		// A DELETE carries only the key columns: its marker gets default values (1970-01-01) for the other
		// sort-key columns, lands under a different key, and the deleted row stays live after FINAL.
		return "its sort key starts with " + tm.OrderBy[0] + ", and a DELETE (retention cleanup included) would otherwise leave the row live in ClickHouse"
	}
	if tm.Mutated && hasToast(t) {
		// With the default identity PeerDB wrote '' for unchanged TOASTed values on every UPDATE.
		return "it is updated and has TOAST-able columns (else unchanged long values arrive empty)"
	}
	return ""
}

// hasToast: extended/external storage columns (text, varchar, jsonb, bytea, arrays) can be TOASTed.
func hasToast(t pgTable) bool {
	for _, c := range t.cols {
		if c.storage == "x" || c.storage == "e" {
			return true
		}
	}
	return false
}

func quoteIdent(parts ...string) string {
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

func Save(path string, p *Plan) error {
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func Load(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	return &p, yaml.Unmarshal(b, &p)
}
