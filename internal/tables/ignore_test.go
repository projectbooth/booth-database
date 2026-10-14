package tables

import (
	"strings"
	"testing"
	"time"
)

func TestIgnore_Matching(t *testing.T) {
	l, err := CompileIgnore(append(append([]string{}, DefaultIgnore...), "staging.*", "raw.tmp_*_old", "exact.Name"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		schema, name string
		want         bool
	}{
		// "*.schema_migrations" matches in every schema, including one with a dot in its name.
		{"public", "schema_migrations", true},
		{"nfl_data", "schema_migrations", true},
		{"a.b", "schema_migrations", true},
		{"public", "alembic_version", true},
		{"public", "flyway_schema_history", true},
		{"app", "django_migrations", true},
		{"public", "__diesel_schema_migrations", true},
		// The whole name must match: no prefix or suffix matches.
		{"public", "schema_migrations_backup", false},
		{"public", "my_schema_migrations", false},
		// Case-sensitive.
		{"public", "Schema_Migrations", false},
		{"exact", "Name", true},
		{"exact", "name", false},
		{"Exact", "Name", false},
		// "*" spans any run, including none and including ".".
		{"staging", "anything", true},
		{"staging", "", true},
		{"staging_2", "x", false},
		{"raw", "tmp__old", true},
		{"raw", "tmp_2024_01_old", true},
		{"raw", "tmp_2024_01_old_x", false},
		{"public", "players", false},
	} {
		if got := l.Ignored(c.schema, c.name); got != c.want {
			t.Errorf("Ignored(%q, %q) = %v, want %v", c.schema, c.name, got, c.want)
		}
	}

	// A pattern that matches nothing is valid and ignores nothing.
	none, err := CompileIgnore([]string{"no_such_schema.no_such_table"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range [][2]string{{"public", "schema_migrations"}, {"no_such_schema", "other"}, {"public", "no_such_table"}} {
		if none.Ignored(k[0], k[1]) {
			t.Errorf("%v matched a pattern that should match nothing", k)
		}
	}

	// Nil and empty ignore nothing.
	var nilList *IgnoreList
	empty, _ := CompileIgnore(nil)
	if nilList.Ignored("public", "schema_migrations") || empty.Ignored("public", "schema_migrations") {
		t.Error("an empty list ignored something")
	}
}

func TestIgnore_BadPatterns(t *testing.T) {
	for _, p := range []string{"", " *.x", "*.x ", "nodot", "*", "a\x00.b"} {
		_, err := CompileIgnore([]string{"*.fine", p})
		if err == nil {
			t.Errorf("%q accepted", p)
			continue
		}
		if !strings.Contains(err.Error(), "pattern") || !strings.Contains(err.Error(), quoteForErr(p)) {
			t.Errorf("%q: error doesn't name the pattern: %v", p, err)
		}
	}
}

func quoteForErr(p string) string {
	q := strings.ReplaceAll(p, "\x00", `\x00`)
	return `"` + q + `"`
}

// An ignored relation never produces created or updated; one already published that becomes
// ignored produces exactly one deleted, then nothing.
func TestIgnore_ReconcileDiff(t *testing.T) {
	l, _ := CompileIgnore(DefaultIgnore)
	now := time.Now()
	mig := rel("public", "schema_migrations", i64(3), "version")
	players := rel("public", "players", i64(10), "id")

	// Never published: only players is created.
	got := summarize(Diff(nil, l.filter([]Relation{mig, players}), now, time.Hour))
	if len(got) != 1 || got[0] != (got1(EventCreated, players)) {
		t.Fatalf("first pass: %v", got)
	}
	// It changes shape and rows: still nothing for it.
	mig2 := rel("public", "schema_migrations", i64(9), "version", "applied_at")
	published := map[Key]Published{players.Key(): pub(players, now)}
	if got := Diff(published, l.filter([]Relation{mig2, players}), now.Add(2*time.Hour), time.Hour); len(got) != 0 {
		t.Fatalf("ignored table republished: %v", summarize(got))
	}

	// Published before it became ignored: deleted once.
	published[mig.Key()] = pub(mig, now)
	got = summarize(Diff(published, l.filter([]Relation{mig, players}), now, time.Hour))
	if len(got) != 1 || got[0] != (got1(EventDeleted, mig)) {
		t.Fatalf("newly ignored: %v", got)
	}
}

func got1(typ string, r Relation) got { return got{Type: typ, Key: r.Key()} }
