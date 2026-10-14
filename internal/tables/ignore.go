package tables

// The table-events ignore list (chart value tableEvents.ignore): relations the reconcile loop
// never publishes as created or updated, such as a migration tool's bookkeeping table. It applies
// to events only. The Tables view, preview and console list and read everything.

import (
	"fmt"
	"strings"
)

// DefaultIgnore is the chart's default tableEvents.ignore: the bookkeeping tables of common
// migration tools, in any schema.
var DefaultIgnore = []string{
	"*.schema_migrations",
	"*.alembic_version",
	"*.flyway_schema_history",
	"*.django_migrations",
	"*.__diesel_schema_migrations",
}

// IgnoreList matches "schema.name" against glob patterns. "*" matches any run of characters
// (including none, and including "."); every other character matches only itself, case-sensitively;
// a pattern must match the whole "schema.name". The zero value (and nil) ignores nothing.
type IgnoreList struct {
	patterns []string
	parts    [][]string // each pattern split on "*"
}

// CompileIgnore validates patterns. A bad pattern is an error naming it: empty, surrounding
// whitespace, a NUL, or no "." (it could then never be a schema.table pattern).
func CompileIgnore(patterns []string) (*IgnoreList, error) {
	l := &IgnoreList{}
	for _, p := range patterns {
		switch {
		case p == "":
			return nil, fmt.Errorf("table events ignore pattern %q: empty", p)
		case strings.TrimSpace(p) != p:
			return nil, fmt.Errorf("table events ignore pattern %q: leading or trailing whitespace", p)
		case strings.ContainsRune(p, 0):
			return nil, fmt.Errorf("table events ignore pattern %q: contains a NUL", p)
		case !strings.Contains(p, "."):
			return nil, fmt.Errorf("table events ignore pattern %q: must have the form schema.table (use * for any schema)", p)
		}
		l.patterns = append(l.patterns, p)
		l.parts = append(l.parts, strings.Split(p, "*"))
	}
	return l, nil
}

// Patterns returns the compiled patterns, in order.
func (l *IgnoreList) Patterns() []string {
	if l == nil {
		return nil
	}
	return append([]string(nil), l.patterns...)
}

// Ignored reports whether schema.name matches any pattern.
func (l *IgnoreList) Ignored(schema, name string) bool {
	if l == nil {
		return false
	}
	s := schema + "." + name
	for _, parts := range l.parts {
		if globMatch(parts, s) {
			return true
		}
	}
	return false
}

// globMatch matches s against a pattern already split on "*": the first part must be a prefix,
// the last a suffix, and the middle parts must appear in order in between. Greedy-leftmost is
// exact for "*"-only globs.
func globMatch(parts []string, s string) bool {
	if len(parts) == 1 {
		return s == parts[0]
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	mid := s[len(first) : len(s)-len(last)]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(mid, p)
		if i < 0 {
			return false
		}
		mid = mid[i+len(p):]
	}
	return true
}

// filter drops ignored relations from a listing.
func (l *IgnoreList) filter(rels []Relation) []Relation {
	if l == nil || len(l.parts) == 0 {
		return rels
	}
	out := make([]Relation, 0, len(rels))
	for _, r := range rels {
		if !l.Ignored(r.Schema, r.Name) {
			out = append(out, r)
		}
	}
	return out
}
