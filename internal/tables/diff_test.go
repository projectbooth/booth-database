package tables

import (
	"reflect"
	"testing"
	"time"
)

func i64(n int64) *int64 { return &n }

func rel(schema, name string, rows *int64, cols ...string) Relation {
	r := Relation{Schema: schema, Name: name, Kind: KindTable, ApproxRows: rows, Columns: []Column{}}
	for _, c := range cols {
		r.Columns = append(r.Columns, Column{Name: c, Type: "integer", Nullable: true})
	}
	r.SchemaHash = SchemaHash(r)
	return r
}

func pub(r Relation, at time.Time) Published {
	return Published{Key: r.Key(), SchemaHash: r.SchemaHash, ApproxRows: r.ApproxRows, PublishedAt: at}
}

type got struct {
	Type string
	Key  Key
}

func summarize(cs []Change) []got {
	out := []got{}
	for _, c := range cs {
		out = append(out, got{c.Type, c.Key})
		if (c.Type == EventDeleted) != (c.Relation == nil) {
			t := c.Type
			panic("a delete must carry no relation and every other event one: " + t)
		}
	}
	return out
}

func TestDiff(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := rel("public", "a", i64(10), "id")
	b := rel("nfl_data", "b", i64(5), "id")
	aWider := rel("public", "a", i64(10), "id", "name")
	aMoreRows := rel("public", "a", i64(11), "id")
	aNoStats := rel("public", "a", nil, "id")

	for _, tc := range []struct {
		name      string
		published map[Key]Published
		current   []Relation
		want      []got
	}{
		{"nothing published: every relation is created", nil, []Relation{a, b},
			[]got{{EventCreated, b.Key()}, {EventCreated, a.Key()}}},
		{"unchanged: nothing", map[Key]Published{a.Key(): pub(a, now)}, []Relation{a}, []got{}},
		{"gone: deleted", map[Key]Published{a.Key(): pub(a, now), b.Key(): pub(b, now)}, []Relation{b},
			[]got{{EventDeleted, a.Key()}}},
		{"column added: updated at once", map[Key]Published{a.Key(): pub(a, now)}, []Relation{aWider},
			[]got{{EventUpdated, a.Key()}}},
		{"row drift inside the gap: nothing", map[Key]Published{a.Key(): pub(a, now.Add(-59*time.Minute))}, []Relation{aMoreRows}, []got{}},
		{"row drift past the gap: updated", map[Key]Published{a.Key(): pub(a, now.Add(-time.Hour))}, []Relation{aMoreRows},
			[]got{{EventUpdated, a.Key()}}},
		{"estimate lost (null) past the gap: updated", map[Key]Published{a.Key(): pub(a, now.Add(-2*time.Hour))}, []Relation{aNoStats},
			[]got{{EventUpdated, a.Key()}}},
		{"deletes first, then creates, then updates, each sorted",
			map[Key]Published{a.Key(): pub(a, now), {"z", "gone"}: {Key: Key{"z", "gone"}, SchemaHash: "x", PublishedAt: now}},
			[]Relation{aWider, b, rel("a", "new", nil)},
			[]got{{EventDeleted, Key{"z", "gone"}}, {EventCreated, Key{"a", "new"}}, {EventCreated, b.Key()}, {EventUpdated, a.Key()}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if g := summarize(Diff(tc.published, tc.current, now, time.Hour)); !reflect.DeepEqual(g, tc.want) {
				t.Errorf("got %v, want %v", g, tc.want)
			}
		})
	}
}

func TestSchemaHash(t *testing.T) {
	base := rel("public", "a", i64(1), "id", "name")
	if SchemaHash(rel("other", "renamed", i64(999), "id", "name")) != SchemaHash(base) {
		t.Error("identity or row count changed the hash")
	}
	comment := "players"
	for name, r := range map[string]Relation{
		"column order":   rel("public", "a", i64(1), "name", "id"),
		"column removed": rel("public", "a", i64(1), "id"),
		"kind":           func() Relation { r := base; r.Kind = KindView; return r }(),
		"comment":        func() Relation { r := base; r.Comment = &comment; return r }(),
		"column type": func() Relation {
			r := base
			r.Columns = append([]Column(nil), base.Columns...)
			r.Columns[0].Type = "bigint"
			return r
		}(),
		"nullability": func() Relation {
			r := base
			r.Columns = append([]Column(nil), base.Columns...)
			r.Columns[0].Nullable = false
			return r
		}(),
	} {
		if SchemaHash(r) == SchemaHash(base) {
			t.Errorf("%s didn't change the hash", name)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"players":                         `"players"`,
		"Mixed Case":                      `"Mixed Case"`,
		`a"b`:                             `"a""b"`,
		`x"; DROP TABLE victim; --`:       `"x""; DROP TABLE victim; --"`,
		`""`:                              `""""""`,
		`public"."other`:                  `"public"".""other"`,
		"ünïcødé":                         `"ünïcødé"`,
		"semi;colon -- and /* comment */": `"semi;colon -- and /* comment */"`,
	} {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
	if got := QualifiedName(`s"`, `t.u`); got != `"s"""."t.u"` {
		t.Errorf("QualifiedName = %s", got)
	}
}

func TestValidIdentifier(t *testing.T) {
	long := ""
	for len(long) < MaxIdentifierBytes {
		long += "x"
	}
	for s, want := range map[string]bool{
		"players": true, `x"; DROP TABLE y; --`: true, long: true, "é": true,
		"": false, long + "x": false, "nul\x00byte": false,
	} {
		if ValidIdentifier(s) != want {
			t.Errorf("ValidIdentifier(%q) = %v, want %v", s, !want, want)
		}
	}
}

func TestSubjectAndMsgID(t *testing.T) {
	if s := Subject("acme", EventCreated); s != "booth.acme.database.table.created" {
		t.Errorf("subject %s", s)
	}
	a := rel("public", "a", i64(1), "id")
	c := Change{Type: EventUpdated, Key: a.Key(), Relation: &a}
	if msgID("acme", c) != msgID("acme", c) {
		t.Error("msgID isn't stable")
	}
	b := rel("public", "a", i64(2), "id")
	if msgID("acme", c) == msgID("acme", Change{Type: EventUpdated, Key: b.Key(), Relation: &b}) {
		t.Error("a different row count must be a different message")
	}
	if msgID("acme", c) == msgID("other", c) {
		t.Error("a different workspace must be a different message")
	}
	nl := Change{Type: EventDeleted, Key: Key{"public", "line\nbreak"}}
	for _, r := range msgID("acme", nl) {
		if r == '\n' || r == '\r' {
			t.Fatal("msgID must be header-safe whatever the table is called")
		}
	}
}
