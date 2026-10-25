import type { Project, ProjectBinding, Relation } from "./types";

// How the Tables tab groups the workspace database's relations: by schema, and by project
// (ADR 0124) above that when booth-core has a project registry.

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

/** booth-core's project registry and its postgres_schema bindings, or null when core has none
 *  (404, 503, any failure): then there are no project headings and ?project= is ignored. */
export interface ProjectIndex {
  projects: Project[];
  bindings: ProjectBinding[];
}

/** A project heading (ADR 0124, contracts/projects.md "Display rules"). */
export interface ProjectNode {
  kind: "project";
  project: Project;
  /** Sorted after active projects and collapsed by default: an archived project, unless it is a
   *  parent with an active sub-project shown under it (archiving doesn't cascade). */
  archivedLast: boolean;
  /** The project's own schemas, shown above its sub-projects. */
  schemas: SchemaGroup[];
  /** Sub-projects (two levels: a sub-project never has its own). */
  children: ProjectNode[];
}

export type ProjectGroup =
  | ProjectNode
  /** Schemas bound to no project: the "No project" heading, last. */
  | { kind: "unassigned"; schemas: SchemaGroup[] }
  /** No heading at all: no registry, or nothing shown is bound to a project. */
  | { kind: "none"; schemas: SchemaGroup[] };

/** Groups schema groups under project headings.
 *
 *  - Bindings match a schema by exact name (kind postgres_schema). A binding to a project the list
 *    doesn't have counts as unbound; a project whose parent isn't in the list is top-level.
 *  - Only projects with schemas (their own or a sub-project's) get a heading.
 *  - `selected` (?project=): that project's schemas and its sub-projects' only, no "No project".
 *    Ignored without a registry, since it can't be resolved.
 *  - Nothing bound and nothing selected: one headless group, exactly as without a registry.
 *
 *  A project is a label: this decides only where a schema is listed, never whether it is. */
export function groupByProject(schemas: SchemaGroup[], index: ProjectIndex | null, selected: string | null = null): ProjectGroup[] {
  if (!index) return [{ kind: "none", schemas }];
  const bySlug = new Map(index.projects.map((p) => [p.slug, p]));
  const boundTo = new Map<string, string>();
  for (const b of index.bindings) if (b.kind === "postgres_schema" && bySlug.has(b.project)) boundTo.set(b.value, b.project);
  const parentOf = (p: Project) => (p.parent && p.parent !== p.slug && bySlug.has(p.parent) ? p.parent : null);

  let shown = schemas;
  if (selected !== null) {
    shown = schemas.filter((g) => {
      const slug = boundTo.get(g.schema);
      if (slug === undefined) return false;
      const p = bySlug.get(slug)!;
      return slug === selected || parentOf(p) === selected;
    });
  } else if (!schemas.some((g) => boundTo.has(g.schema))) {
    return [{ kind: "none", schemas }];
  }

  const own = new Map<string, SchemaGroup[]>();
  const unassigned: SchemaGroup[] = [];
  for (const g of shown) {
    const slug = boundTo.get(g.schema);
    if (slug === undefined) unassigned.push(g);
    else own.set(slug, [...(own.get(slug) ?? []), g]);
  }

  const node = (p: Project, children: ProjectNode[]): ProjectNode => ({
    kind: "project",
    project: p,
    archivedLast: p.archived && !children.some((c) => !c.project.archived),
    schemas: own.get(p.slug) ?? [],
    children,
  });
  const order = (a: ProjectNode, b: ProjectNode) =>
    Number(a.archivedLast) - Number(b.archivedLast) || a.project.name.localeCompare(b.project.name) || a.project.slug.localeCompare(b.project.slug);

  const top: ProjectNode[] = [];
  for (const p of index.projects) {
    if (parentOf(p) !== null) continue;
    const children = index.projects
      .filter((c) => parentOf(c) === p.slug && own.has(c.slug))
      .map((c) => node(c, []))
      .sort(order);
    if (own.has(p.slug) || children.length > 0) top.push(node(p, children));
  }
  top.sort(order);

  const groups: ProjectGroup[] = top;
  if (unassigned.length > 0) groups.push({ kind: "unassigned", schemas: unassigned });
  return groups;
}

/** Every relation under a project heading, its sub-projects' included. */
export function relationsOf(g: ProjectGroup): Relation[] {
  const mine = g.schemas.flatMap((s) => s.relations);
  return g.kind === "project" ? [...mine, ...g.children.flatMap(relationsOf)] : mine;
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
