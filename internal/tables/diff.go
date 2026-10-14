package tables

import (
	"sort"
	"time"
)

// Event types published on the bus (ADR 0116 §2): namespaced under "database." so they can never
// be confused with booth-lakehouse's table.* (ADR 0085).
const (
	EventCreated = "database.table.created"
	EventUpdated = "database.table.updated"
	EventDeleted = "database.table.deleted"
)

// DefaultRowCountRepublishGap is how often a change in ApproxRows alone may republish a table
// (ADR 0116 brief: "row-count drift alone does not republish more than once an hour"). A schema
// change always republishes on the next pass.
const DefaultRowCountRepublishGap = time.Hour

// Published is what was last published (and acknowledged by the stream) for one relation.
type Published struct {
	Key
	SchemaHash  string
	ApproxRows  *int64
	PublishedAt time.Time
}

// Change is one event to publish. Relation is nil for a delete.
type Change struct {
	Type     string
	Key      Key
	Relation *Relation
}

// Diff compares the current listing with what was last published and returns the events to
// publish: deletes first, then creates, then updates, each sorted by (schema, name), so a
// dropped-and-recreated name is never momentarily registered twice and the output is stable.
//
//   - in current, not published → created;
//   - published, not in current → deleted;
//   - in both, schema hash differs → updated;
//   - in both, same hash, ApproxRows differs, and the last publish is at least rowGap old →
//     updated (row-count drift alone republishes at most once per rowGap).
func Diff(published map[Key]Published, current []Relation, now time.Time, rowGap time.Duration) []Change {
	cur := make(map[Key]*Relation, len(current))
	for i := range current {
		cur[current[i].Key()] = &current[i]
	}
	var deletes, creates, updates []Change
	for k := range published {
		if _, ok := cur[k]; !ok {
			deletes = append(deletes, Change{Type: EventDeleted, Key: k})
		}
	}
	for k, r := range cur {
		p, ok := published[k]
		switch {
		case !ok:
			creates = append(creates, Change{Type: EventCreated, Key: k, Relation: r})
		case p.SchemaHash != r.SchemaHash:
			updates = append(updates, Change{Type: EventUpdated, Key: k, Relation: r})
		case !sameCount(p.ApproxRows, r.ApproxRows) && now.Sub(p.PublishedAt) >= rowGap:
			updates = append(updates, Change{Type: EventUpdated, Key: k, Relation: r})
		}
	}
	for _, s := range [][]Change{deletes, creates, updates} {
		sort.Slice(s, func(i, j int) bool {
			if s[i].Key.Schema != s[j].Key.Schema {
				return s[i].Key.Schema < s[j].Key.Schema
			}
			return s[i].Key.Name < s[j].Key.Name
		})
	}
	return append(append(deletes, creates...), updates...)
}

func sameCount(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
