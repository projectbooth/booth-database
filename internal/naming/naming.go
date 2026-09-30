// Package naming derives every PostgreSQL identifier booth-database creates from a workspace
// slug or a lease id — the only place those names are decided, so the provisioner, the reaper
// and the backup job can never disagree about them.
//
// Workspace databases are named from a hash of the slug, not the slug itself
// (docs/decisions/0001 §2): PostgreSQL's pg_database and pg_roles catalogs are readable by any
// connected role on the server, so a slug-named database would let one workspace's credential
// list every other workspace on the deployment — the same enumeration ADR 0052 already rules
// out for the user directory. A hash still leaks how many workspaces have a database, never
// which ones.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// workspacePattern is ADR 0025's workspace-slug grammar (the `/workspaces/<slug>/<role>` groups
// claim), which is what booth-core forwards as the requester's workspace.
var workspacePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// maxWorkspaceLen bounds a slug purely as input hygiene; the derived names are fixed-length.
const maxWorkspaceLen = 200

const (
	// DatabasePrefix starts every workspace database's name. The backup job selects databases by
	// exactly this prefix (charts/booth-database/templates/backup-cronjob.yaml).
	DatabasePrefix = "bdb_ws_"
	// LeasePrefix starts every short-lived login role minted for one credential-broker lease. The
	// reaper only ever drops roles with this prefix.
	LeasePrefix = "bdb_lease_"

	readWriteSuffix = "_rw"
	readOnlySuffix  = "_ro"
)

// ValidWorkspace reports whether ws is a well-formed workspace slug.
func ValidWorkspace(ws string) bool {
	return len(ws) <= maxWorkspaceLen && workspacePattern.MatchString(ws)
}

// Workspace is the set of names belonging to one workspace's database.
type Workspace struct {
	// Slug is the workspace this is for. Never written into PostgreSQL anywhere.
	Slug string
	// Database is the workspace's own database.
	Database string
	// ReadWrite is the NOLOGIN group role that owns the database and everything in it; a
	// readwrite lease is a member of it.
	ReadWrite string
	// ReadOnly is the NOLOGIN group role with SELECT on everything ReadWrite creates; a read
	// lease is a member of it.
	ReadOnly string
}

// ForWorkspace derives a workspace's names. The hash is domain-separated so the same slug can't
// collide with some other use of SHA-256 over it. 24 hex characters (96 bits) keeps every name
// well under PostgreSQL's 63-byte identifier limit, with collisions not a practical concern.
func ForWorkspace(ws string) (Workspace, error) {
	if !ValidWorkspace(ws) {
		return Workspace{}, fmt.Errorf("invalid workspace slug %q", ws)
	}
	sum := sha256.Sum256([]byte("booth-database/workspace/" + ws))
	db := DatabasePrefix + hex.EncodeToString(sum[:])[:24]
	return Workspace{Slug: ws, Database: db, ReadWrite: db + readWriteSuffix, ReadOnly: db + readOnlySuffix}, nil
}

// GroupFor returns the group role a lease with the given access joins.
func (w Workspace) GroupFor(readWrite bool) string {
	if readWrite {
		return w.ReadWrite
	}
	return w.ReadOnly
}

// DatabaseForGroup inverts GroupFor: the database a group role belongs to, or "" if name isn't
// one of this module's group roles. Used by the reaper, which only has a lease role's group
// membership to go on (the slug is never stored).
func DatabaseForGroup(name string) string {
	if !strings.HasPrefix(name, DatabasePrefix) {
		return ""
	}
	for _, suffix := range []string{readWriteSuffix, readOnlySuffix} {
		if db, ok := strings.CutSuffix(name, suffix); ok && len(db) == len(DatabasePrefix)+24 {
			return db
		}
	}
	return ""
}

// ReadWriteGroup returns the readwrite group role for a workspace database name.
func ReadWriteGroup(database string) string { return database + readWriteSuffix }

// LeaseRole is the login role for one lease. Derived from the lease id so the broker's audit
// trail (which records the lease id) and PostgreSQL's own logs (which record the role) join up.
func LeaseRole(leaseID string) (string, error) {
	id := strings.ReplaceAll(strings.ToLower(leaseID), "-", "")
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", fmt.Errorf("lease id %q is not a UUID", leaseID)
	}
	return LeasePrefix + id, nil
}
