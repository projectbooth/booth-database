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

// ADR 0116: the workspace database's contents. Mirrors internal/tables and internal/query.

export type RelationKind = "table" | "view" | "materialized_view";

export interface TableColumn {
  name: string;
  /** PostgreSQL's format_type() rendering, e.g. "character varying(20)". */
  type: string;
  nullable: boolean;
  comment: string | null;
}

/** One table, view or materialized view (also the database.table.* event payload). */
export interface Relation {
  schema: string;
  name: string;
  kind: RelationKind;
  comment: string | null;
  columns: TableColumn[];
  /** The planner's estimate, null when there is none (never analyzed) or for a view. Show as "about N". */
  approxRows: number | null;
  schemaHash: string;
  /** ADR 0117: the API write grants that can change this table, from the database's live
   *  privileges. Absent when none. Only in GET /api/tables, never in table events. */
  writableThrough?: WriteAccess[];
}

/** One API write grant's live privileges on a table. */
export interface WriteAccess {
  /** booth-api's grant id. */
  grant: string;
  /** The API the grant belongs to, when the grant id names one ("api:<id>"). */
  api?: string;
  operations: ("insert" | "update" | "delete")[];
  /** The columns it may insert or update. */
  columns: string[];
}

/** GET /api/tables */
export interface TablesResponse {
  workspace: string;
  provisioned: boolean;
  tables: Relation[];
}

/** A rendered value: null, a boolean, a number (int2/int4/float), or PostgreSQL's own text. */
export type CellValue = string | number | boolean | null;

/** A preview's or console query's result. */
export interface QueryResult {
  columns: { name: string; type: string }[];
  rows: CellValue[][];
  rowCount: number;
  truncated: boolean;
  truncatedReason?: "rows" | "bytes" | "value";
  commandTag: string;
  durationMs: number;
}
