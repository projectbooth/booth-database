// Mirrors the JSON shapes in internal/api and internal/provision/status.go — hand-written, like
// every other native module's UI package (no shared-schema tooling). Keep in sync by hand.

/** Caller's role in the active workspace (ADR 0025), per the NativeModuleProps contract (ADR 0031). */
export type WorkspaceRole = "owner" | "editor" | "viewer";

/** One workspace database's status — only what PostgreSQL reports cheaply. */
export interface DatabaseStatus {
  /** The database's own name, a hash of the workspace slug (booth-database never stores slugs). */
  database: string;
  /** RFC 3339; null for a database provisioned before creation times were recorded. */
  createdAt: string | null;
  sizeBytes: number;
  /** Unexpired credentials issued for this database. */
  activeCredentials: number;
  openConnections: number;
  /** Only present for the caller's own workspace (it needs a connection into the database). */
  tables?: number;
}

/** GET /api/status */
export interface StatusResponse {
  workspace: string;
  /** False until someone in the workspace first asks for a credential (databases are created lazily). */
  provisioned: boolean;
  database: DatabaseStatus | null;
  /** Whether the caller may see the cross-workspace listing (GET /api/databases). */
  operator: boolean;
}

/** One row of GET /api/databases. */
export interface ListedDatabase extends DatabaseStatus {
  /** Set only for a workspace the caller belongs to. */
  workspace?: string;
}
