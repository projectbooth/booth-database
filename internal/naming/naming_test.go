package naming

import (
	"strings"
	"testing"
)

func TestForWorkspace(t *testing.T) {
	a, err := ForWorkspace("acme-analytics")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ForWorkspace("acme-analytics")
	if a != again {
		t.Fatal("names must be deterministic: the reaper and backup job re-derive them")
	}
	b, _ := ForWorkspace("acme")
	if a.Database == b.Database {
		t.Fatal("distinct workspaces got the same database")
	}
	for _, name := range []string{a.Database, a.ReadWrite, a.ReadOnly} {
		if len(name) > 63 {
			t.Errorf("%q exceeds PostgreSQL's 63-byte identifier limit", name)
		}
		if strings.Contains(name, "acme") {
			t.Errorf("%q reveals the workspace slug", name)
		}
		if !strings.HasPrefix(name, DatabasePrefix) {
			t.Errorf("%q lacks the prefix the backup job selects on", name)
		}
	}
	if a.GroupFor(true) != a.ReadWrite || a.GroupFor(false) != a.ReadOnly {
		t.Error("GroupFor mixed up the groups")
	}
	if DatabaseForGroup(a.ReadWrite) != a.Database || DatabaseForGroup(a.ReadOnly) != a.Database {
		t.Error("DatabaseForGroup doesn't invert GroupFor")
	}
	if ReadWriteGroup(a.Database) != a.ReadWrite {
		t.Error("ReadWriteGroup doesn't match ForWorkspace")
	}
}

func TestForWorkspace_Long(t *testing.T) {
	n, err := ForWorkspace(strings.Repeat("a", 200))
	if err != nil {
		t.Fatal(err)
	}
	if len(n.ReadWrite) > 63 {
		t.Fatalf("long slug produced a %d-byte name", len(n.ReadWrite))
	}
}

func TestValidWorkspace(t *testing.T) {
	for _, ok := range []string{"acme", "acme-analytics", "a1", "0"} {
		if !ValidWorkspace(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "Acme", "acme_analytics", "../x", "a b", "a;drop", strings.Repeat("a", 201)} {
		if ValidWorkspace(bad) {
			t.Errorf("accepted %q", bad)
		}
		if _, err := ForWorkspace(bad); err == nil {
			t.Errorf("ForWorkspace accepted %q", bad)
		}
	}
}

func TestDatabaseForGroup_RejectsOthers(t *testing.T) {
	for _, s := range []string{"", "postgres", "bdb_ws_short_rw", "bdb_lease_0123", "other_rw"} {
		if DatabaseForGroup(s) != "" {
			t.Errorf("DatabaseForGroup(%q) matched", s)
		}
	}
}

func TestLeaseRole(t *testing.T) {
	r, err := LeaseRole("0F8FAD5B-D9CB-469F-A165-70867728950E")
	if err != nil {
		t.Fatal(err)
	}
	if r != "bdb_lease_0f8fad5bd9cb469fa16570867728950e" || len(r) > 63 {
		t.Fatalf("LeaseRole = %q", r)
	}
	for _, bad := range []string{"", "x", "0f8fad5b-d9cb-469f-a165-70867728950", "zz8fad5b-d9cb-469f-a165-70867728950e", `0f8fad5b"; drop role x; --0000000`} {
		if _, err := LeaseRole(bad); err == nil {
			t.Errorf("LeaseRole accepted %q", bad)
		}
	}
}
