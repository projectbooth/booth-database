import type { Relation } from "./types";

// How the Tables tab groups the workspace database's relations: by schema, and (later) by
// project above that.

/** System schemas: shown last and collapsed by default. The one place this list lives.
 *
 *  None of these reach the Tables tab today: the server never lists information_schema or a pg_*
 *  schema (internal/tables ExcludedSchemas), and this module keeps its own `booth_database` state in
 *  the server's maintenance database, not a workspace database (docs/decisions/0006 §1). The list is
 *  here so that if one ever is listed, it can't crowd out the workspace's own schemas. Extension
 *  schemas are deliberately not guessed at. */
const SYSTEM_SCHEMA_NAMES: readonly string[] = ["booth_database", "information_schema", "pg_catalog", "pg_toast"];
const SYSTEM_SCHEMA_PATTERN = /^pg_(toast_)?temp_\d+$/;

export function isSystemSchema(schema: string): boolean {
  return SYSTEM_SCHEMA_NAMES.includes(schema) || SYSTEM_SCHEMA_PATTERN.test(schema);
}

export interface SchemaGroup {
  schema: string;
  system: boolean;
  relations: Relation[];
}

/** Groups relations by schema: the workspace's schemas by name, then system schemas. Relations
 *  keep their incoming order (the server sorts by schema, then name). */
export function groupBySchema(relations: Relation[]): SchemaGroup[] {
  const bySchema = new Map<string, Relation[]>();
  for (const r of relations) {
    const list = bySchema.get(r.schema);
    if (list) list.push(r);
    else bySchema.set(r.schema, [r]);
  }
  return [...bySchema.entries()]
    .map(([schema, rels]) => ({ schema, system: isSystemSchema(schema), relations: rels }))
    .sort((a, b) => Number(a.system) - Number(b.system) || a.schema.localeCompare(b.schema));
}

/**
 * PROJECT HOOK (ADR 0124). A project is a navigation label above schemas (e.g. finance >
 * finance-dev), bound to Postgres schemas by booth-core's project registry. That registry doesn't
 * exist yet, so this returns one group with no project and the view shows no project heading.
 *
 * When the registry lands, this is the one place that changes: look up each schema's bound
 * project, group by it (children under their parent, "No project" for the rest), and the view
 * renders a heading per non-null `project`. A project is never used to decide access.
 */
export interface ProjectGroup {
  /** The project's slug; null for "no project heading" (today, always). */
  project: string | null;
  schemas: SchemaGroup[];
}

export function groupByProject(schemas: SchemaGroup[]): ProjectGroup[] {
  return [{ project: null, schemas }];
}

/** "3 tables", "1 table, 2 views": materialized views count as views. */
export function countLabel(relations: Relation[]): string {
  const views = relations.filter((r) => r.kind !== "table").length;
  const tables = relations.length - views;
  const parts: string[] = [];
  if (tables > 0 || views === 0) parts.push(`${tables} ${tables === 1 ? "table" : "tables"}`);
  if (views > 0) parts.push(`${views} ${views === 1 ? "view" : "views"}`);
  return parts.join(", ");
}
