// guard-id: bossd-schema-names

package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/migrations"
)

type sqliteColumn struct{ table, name, declType, dflt string }

//go:embed testdata/schema_naming_baseline.json
var schemaNamingBaseline []byte

func schemaBoolPrefix(name string) bool {
	for _, prefix := range []string{"is_", "has_", "should_", "can_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func schemaQuantityOrEnum(name string) bool {
	for _, bare := range []string{"priority", "state", "status", "result", "trigger", "version", "depth", "rank", "seq"} {
		if name == bare {
			return true
		}
	}
	for _, suffix := range []string{"_count", "_seconds", "_ms", "_bytes", "_id", "_version", "_depth", "_priority", "_state", "_status", "_leg", "_intent", "_result", "_trigger", "_number", "_index", "_rank", "_seq"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

var schemaTimeDefault = regexp.MustCompile(`(?i)\b(?:strftime|datetime)\s*\(|\bCURRENT_TIMESTAMP\b`)

func schemaNamingViolations(cols []sqliteColumn) []string {
	var violations []string
	for _, c := range cols {
		typ := strings.ToUpper(strings.TrimSpace(c.declType))
		integer := strings.Contains(typ, "INT")
		def := strings.TrimSpace(c.dflt)
		for strings.HasPrefix(def, "(") && strings.HasSuffix(def, ")") {
			def = strings.TrimSpace(def[1 : len(def)-1])
		}
		flag := integer && (def == "0" || def == "1") && !schemaQuantityOrEnum(c.name)
		add := func(rule string) { violations = append(violations, c.table+"."+c.name+": "+rule) }
		if (typ == "BOOLEAN" || typ == "BOOL" || flag) && !schemaBoolPrefix(c.name) {
			add("bool-prefix")
		}
		if schemaBoolPrefix(c.name) && !integer {
			add("bool-type")
		}
		timestamp := typ == "DATETIME" || strings.HasPrefix(typ, "TIMESTAMP") || c.name == "timestamp"
		for _, suffix := range []string{"_until", "_since", "_time", "_timestamp"} {
			timestamp = timestamp || strings.HasSuffix(c.name, suffix)
		}
		timestamp = timestamp || schemaTimeDefault.MatchString(c.dflt)
		if timestamp && !strings.HasSuffix(c.name, "_at") {
			add("timestamp-suffix")
		}
		if typ == "DATE" && !strings.HasSuffix(c.name, "_on") {
			add("date-suffix")
		}
		if strings.HasSuffix(c.name, "_at") && typ != "TEXT" {
			add("timestamp-type")
		}
	}
	slices.Sort(violations)
	return violations
}

func readSQLiteColumns(t *testing.T, database *sql.DB) []sqliteColumn {
	t.Helper()
	rows, err := database.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var cols []sqliteColumn
	for _, table := range tables {
		rows, err := database.Query("PRAGMA table_info(" + quoteSQLiteIdentifier(table) + ")")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var def sql.NullString
			if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, sqliteColumn{table, name, typ, def.String})
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return cols
}

func quoteSQLiteIdentifier(name string) string {
	return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
}

func schemaBaselineFailures(data []byte, offenders []string) []string {
	var raw []any
	if err := json.Unmarshal(data, &raw); err != nil {
		return []string{err.Error()}
	}
	if raw == nil {
		return []string{"baseline must be a JSON array of strings"}
	}
	entries := make([]string, len(raw))
	for i, entry := range raw {
		var ok bool
		entries[i], ok = entry.(string)
		if !ok {
			return []string{"baseline must be a JSON array of strings"}
		}
	}
	var failures []string
	baseline := map[string]bool{}
	for i, entry := range entries {
		if i > 0 && entries[i-1] > entry {
			failures = append(failures, "baseline must stay sorted")
		}
		if baseline[entry] {
			failures = append(failures, "baseline has duplicate entries")
		}
		baseline[entry] = true
	}
	current := map[string]bool{}
	for _, offender := range offenders {
		key, _, _ := strings.Cut(offender, ": ")
		current[key] = true
		if !baseline[key] {
			failures = append(failures, offender)
		}
	}
	for entry := range baseline {
		if !current[entry] {
			failures = append(failures, entry+" no longer occurs — remove it")
		}
	}
	slices.Sort(failures)
	return failures
}

func TestSchemaNamingAtHead(t *testing.T) {
	cols := readSQLiteColumns(t, dbtest.NewMigrated(t))
	if len(cols) == 0 {
		t.Fatal("no application columns read")
	}
	if failures := schemaBaselineFailures(schemaNamingBaseline, schemaNamingViolations(cols)); len(failures) > 0 {
		t.Fatal(strings.Join(failures, "\n"))
	}
}

func TestSchemaNamingFailsOnSeededBareBoolean(t *testing.T) {
	overlay := fstest.MapFS{}
	err := fs.WalkDir(migrations.FS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return err
		}
		overlay[name] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	overlay["99991231000000_seed_bare_boolean.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE repos ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;\n")}
	database := dbtest.NewEmpty(t)
	if err := dbtest.Run(database, overlay); err != nil {
		t.Fatal(err)
	}
	violations := schemaNamingViolations(readSQLiteColumns(t, database))
	if !slices.Contains(violations, "repos.archived: bool-prefix") {
		t.Fatalf("seeded flag not reported: %v", violations)
	}
}

func TestSchemaNamingRules(t *testing.T) {
	tests := []struct {
		name, typ, def string
		want           []string
	}{
		{"archived", "BOOLEAN", "", []string{"bool-prefix"}},
		{"archived", "BOOL", "", []string{"bool-prefix"}},
		{"archived", "INTEGER", "0", []string{"bool-prefix"}},
		{"archived", "BIGINT", "(1)", []string{"bool-prefix"}},
		{"is_archived", "TEXT", "", []string{"bool-type"}},
		{"is_archived", "INTEGER", "0", nil},
		{"has_access", "INT", "1", nil},
		{"should_run", "INTEGER", "0", nil},
		{"can_run", "INTEGER", "1", nil},
		{"created", "DATETIME", "", []string{"timestamp-suffix"}},
		{"created", "TIMESTAMP", "", []string{"timestamp-suffix"}},
		{"created", "TEXT", "(strftime('%Y-%m-%d','now'))", []string{"timestamp-suffix"}},
		{"created", "TEXT", "datetime('now')", []string{"timestamp-suffix"}},
		{"created", "TEXT", "CURRENT_TIMESTAMP", []string{"timestamp-suffix"}},
		{"created_at", "INTEGER", "", []string{"timestamp-type"}},
		{"created_at", "TEXT", "", nil},
		{"birthday", "DATE", "", []string{"date-suffix"}},
		{"birthday_on", "DATE", "", nil},
		{"ordinary", "TEXT", "'0'", nil},
		{"ordinary", "INTEGER", "2", nil},
	}
	for _, name := range []string{"cooldown_until", "active_since", "start_time", "event_timestamp", "timestamp"} {
		tests = append(tests, struct {
			name, typ, def string
			want           []string
		}{name, "TEXT", "", []string{"timestamp-suffix"}})
	}
	for _, suffix := range []string{"count", "seconds", "ms", "bytes", "id", "version", "depth", "priority", "state", "status", "leg", "intent", "result", "trigger", "number", "index", "rank", "seq"} {
		tests = append(tests, struct {
			name, typ, def string
			want           []string
		}{"thing_" + suffix, "INTEGER", "0", nil})
	}
	for _, name := range []string{"priority", "state", "status", "result", "trigger", "version", "depth", "rank", "seq"} {
		tests = append(tests, struct {
			name, typ, def string
			want           []string
		}{name, "INTEGER", "1", nil})
	}
	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			got := schemaNamingViolations([]sqliteColumn{{"example", tt.name, tt.typ, tt.def}})
			var want []string
			for _, rule := range tt.want {
				want = append(want, "example."+tt.name+": "+rule)
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestSchemaNamingBaseline(t *testing.T) {
	for _, tt := range []struct {
		name, data  string
		offenders   []string
		wantFailure bool
	}{
		{"matching", `["a.b"]`, []string{"a.b: bool-prefix"}, false},
		{"new", `[]`, []string{"a.b: bool-prefix"}, true},
		{"stale", `["a.b"]`, nil, true},
		{"unsorted", `["b.c","a.b"]`, []string{"a.b: bool-prefix", "b.c: bool-prefix"}, true},
		{"duplicate", `["a.b","a.b"]`, []string{"a.b: bool-prefix"}, true},
		{"invalid", `{}`, nil, true},
		{"null", `null`, nil, true},
		{"null element", `[null]`, nil, true},
		{"non-string", `[1]`, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := schemaBaselineFailures([]byte(tt.data), tt.offenders)
			if (len(got) > 0) != tt.wantFailure {
				t.Fatalf("failures: %v", got)
			}
		})
	}
}
