import { useId, useMemo, useState, type ReactNode } from "react";
import { listTables, previewTable, type ApiContext } from "./api/client";
import { Alert, Link, Muted, Panel, ResultGrid, ResultSummary, tableEl, tableShell, tbody, td, th, thead, WritableBadge } from "./components";
import { formatApproxRows, kindLabel } from "./format";
import { countLabel, groupByProject, groupBySchema, relationsOf, type ProjectGroup, type ProjectIndex, type ProjectNode, type SchemaGroup } from "./grouping";
import { help } from "./help";
import { loadProjectIndex, useLoad, type LoadState } from "./hooks";
import { useProjectParam } from "./navigation";
import type { Relation, TablesResponse } from "./types";

/** Puts a bounded SELECT of one table in the docked console (DatabaseApp owns the dock). */
export type QueryTable = (schema: string, table: string) => void;

/** The Tables tab (ADR 0116): every table, view and materialized view in the workspace database,
 *  grouped by schema, for any member. Opening it also asks the server to check for changes for
 *  the Catalog. */
export function TablesView({ api, onQuery }: { api: ApiContext; onQuery: QueryTable }) {
  const { state, reload: reloadTables } = useLoad(() => listTables(api), [api]);
  // ADR 0124: booth-core's projects, or null without a registry. Fetched alongside the tables;
  // the list waits for it (at most PROJECTS_TIMEOUT_MS) so it doesn't regroup under the reader.
  const projects = useLoad(() => loadProjectIndex(api), [api]);
  const selected = useProjectParam();
  const [q, setQ] = useState("");
  const reload = () => {
    reloadTables();
    projects.reload();
  };

  return (
    <section aria-labelledby="tables-heading" className="flex flex-col gap-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <h2 id="tables-heading" className="text-sm font-semibold">
          Tables
        </h2>
        <div className="flex items-center gap-2">
          <input
            type="search"
            value={q}
            onChange={(ev) => setQ(ev.target.value)}
            placeholder="Search tables and columns"
            aria-label="Search tables and columns"
            className="w-64 rounded-md border border-slate-300 bg-white px-2 py-1.5 text-sm text-slate-900 placeholder:text-slate-400 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-100 dark:placeholder:text-slate-500"
          />
          <button
            type="button"
            onClick={reload}
            className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            Refresh
          </button>
        </div>
      </div>
      <TablesBody
        state={projects.state.status === "loading" ? projects.state : state}
        index={projects.state.status === "ready" ? projects.state.data : null}
        selected={selected}
        q={q}
        onQuery={onQuery}
      />
    </section>
  );
}

function matches(r: Relation, q: string): boolean {
  if (!q) return true;
  const needle = q.toLowerCase();
  return `${r.schema}.${r.name}`.toLowerCase().includes(needle) || r.columns.some((c) => c.name.toLowerCase().includes(needle));
}

function TablesBody({
  state,
  index,
  selected,
  q,
  onQuery,
}: {
  state: LoadState<TablesResponse>;
  index: ProjectIndex | null;
  selected: string | null;
  q: string;
  onQuery: QueryTable;
}) {
  const needle = q.trim();
  const groups = useMemo(
    () => (state.status === "ready" ? groupByProject(groupBySchema(state.data.tables.filter((r) => matches(r, needle))), index, selected) : []),
    [state, needle, index, selected],
  );
  // Headings the user has toggled from their default ("s:<schema>", "p:<slug>", "u" for No
  // project). System schemas and archived projects start collapsed, the rest open. A search shows
  // every match whatever is collapsed, so filtering always works across all schemas.
  const [toggled, setToggled] = useState<ReadonlySet<string>>(() => new Set());
  const toggle = (key: string) =>
    setToggled((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  const isOpen = (key: string, collapsedByDefault: boolean) => needle !== "" || collapsedByDefault === toggled.has(key);
  const ctx: GroupContext = { isOpen, toggle, onQuery };

  if (state.status === "loading") return <Muted>Loading tables…</Muted>;
  if (state.status === "error") return <Alert>Couldn&apos;t load tables: {state.error}</Alert>;
  if (!state.data.provisioned) {
    return (
      <Panel>
        <p className="text-sm font-medium">No database yet</p>
        <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">It&apos;s created automatically the first time code in this workspace connects.</p>
      </Panel>
    );
  }
  if (state.data.tables.length === 0) return <Muted>This database has no tables yet. Create them from a notebook or pipeline task.</Muted>;
  const filtering = index !== null && selected !== null;
  const selectedProject = filtering ? index.projects.find((p) => p.slug === selected) : undefined;
  const empty = groups.every((g) => relationsOf(g).length === 0);
  return (
    <>
      {filtering && (
        <p className="text-xs text-slate-500 dark:text-slate-400">
          {selectedProject ? (
            <>
              Showing project <span className="font-medium">{selectedProject.name}</span> and its sub-projects.
            </>
          ) : (
            <>There&apos;s no project called {selected}.</>
          )}{" "}
          Projects only group this list; they don&apos;t change who can see what.
        </p>
      )}
      {groups.map((g) => (
        <GroupView key={g.kind === "project" ? `p:${g.project.slug}` : g.kind} group={g} ctx={ctx} />
      ))}
      {empty && (needle ? <Muted>Nothing matches &quot;{q}&quot;.</Muted> : <Muted>No tables in this project.</Muted>)}
      <p className="text-xs text-slate-500 dark:text-slate-400">
        Row counts are PostgreSQL&apos;s estimates and can lag behind recent changes.
      </p>
    </>
  );
}

interface GroupContext {
  isOpen: (key: string, collapsedByDefault: boolean) => boolean;
  toggle: (key: string) => void;
  onQuery: QueryTable;
}

type Level = 3 | 4 | 5;

const HEADING = { 3: "h3", 4: "h4", 5: "h5" } as const;

function Schemas({ schemas, level, ctx }: { schemas: SchemaGroup[]; level: Level; ctx: GroupContext }) {
  return (
    <>
      {schemas.map((g) => (
        <SchemaSection
          key={g.schema}
          group={g}
          level={level}
          open={ctx.isOpen(`s:${g.schema}`, g.system)}
          onToggle={() => ctx.toggle(`s:${g.schema}`)}
          onQuery={ctx.onQuery}
        />
      ))}
    </>
  );
}

/** One top-level group: schemas with no heading, a project, or "No project". */
function GroupView({ group, ctx }: { group: ProjectGroup; ctx: GroupContext }) {
  if (group.kind === "none") {
    return (
      <div className="flex flex-col gap-3">
        <Schemas schemas={group.schemas} level={3} ctx={ctx} />
      </div>
    );
  }
  if (group.kind === "project") return <ProjectSection node={group} level={3} ctx={ctx} />;
  return (
    <Collapsible
      label="No project"
      level={3}
      open={ctx.isOpen("u", false)}
      onToggle={() => ctx.toggle("u")}
      title={
        <span className="font-semibold" {...help("noProject")}>
          No project
        </span>
      }
      count={countLabel(relationsOf(group))}
    >
      <Schemas schemas={group.schemas} level={4} ctx={ctx} />
    </Collapsible>
  );
}

/** A project heading: its own schemas, then its sub-projects. */
function ProjectSection({ node, level, ctx }: { node: ProjectNode; level: 3 | 4; ctx: GroupContext }) {
  const key = `p:${node.project.slug}`;
  return (
    <Collapsible
      label={`Project ${node.project.name}`}
      level={level}
      open={ctx.isOpen(key, node.archivedLast)}
      onToggle={() => ctx.toggle(key)}
      title={
        <span className="font-semibold" {...help("project")}>
          {node.project.name}
        </span>
      }
      count={countLabel(relationsOf(node))}
      marker={node.project.archived ? "archived" : undefined}
    >
      <Schemas schemas={node.schemas} level={level === 3 ? 4 : 5} ctx={ctx} />
      {node.children.map((c) => (
        <ProjectSection key={c.project.slug} node={c} level={4} ctx={ctx} />
      ))}
    </Collapsible>
  );
}

function Collapsible({
  label,
  level,
  open,
  onToggle,
  title,
  count,
  marker,
  children,
}: {
  label: string;
  level: Level;
  open: boolean;
  onToggle: () => void;
  title: ReactNode;
  count: string;
  marker?: string;
  children: ReactNode;
}) {
  const bodyId = useId();
  const H = HEADING[level];
  return (
    <section aria-label={label} className="flex flex-col gap-3">
      <H className="text-sm">
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={open}
          aria-controls={bodyId}
          className="flex items-center gap-2 rounded-md px-1 py-0.5 text-left hover:bg-slate-100 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:hover:bg-slate-800"
        >
          <span aria-hidden="true" className="w-3 text-slate-400 dark:text-slate-500">
            {open ? "▾" : "▸"}
          </span>
          {title}
          <span className="text-xs text-slate-500 dark:text-slate-400">{count}</span>
          {marker && <span className="text-xs text-slate-500 dark:text-slate-400">· {marker}</span>}
        </button>
      </H>
      <div id={bodyId} hidden={!open} className="flex flex-col gap-3 border-l border-slate-200 pl-4 dark:border-slate-700">
        {children}
      </div>
    </section>
  );
}

function SchemaSection({
  group,
  level,
  open,
  onToggle,
  onQuery,
}: {
  group: SchemaGroup;
  level: Level;
  open: boolean;
  onToggle: () => void;
  onQuery: QueryTable;
}) {
  const bodyId = useId();
  const H = HEADING[level];
  return (
    <section aria-label={`Schema ${group.schema}`} className="flex flex-col gap-2">
      <H className="text-sm">
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={open}
          aria-controls={bodyId}
          className="flex items-center gap-2 rounded-md px-1 py-0.5 text-left hover:bg-slate-100 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:hover:bg-slate-800"
        >
          <span aria-hidden="true" className="w-3 text-slate-400 dark:text-slate-500">
            {open ? "▾" : "▸"}
          </span>
          <span className="font-mono font-semibold" {...help("schema")}>
            {group.schema}
          </span>
          <span className="text-xs text-slate-500 dark:text-slate-400">{countLabel(group.relations)}</span>
          {group.system && <span className="text-xs text-slate-500 dark:text-slate-400">· system</span>}
        </button>
      </H>
      <div id={bodyId} hidden={!open} className={tableShell}>
        <table className={tableEl}>
          <thead className={thead}>
            <tr>
              <th className={th} {...help("name")}>
                Name
              </th>
              <th className={th} {...help("kind")}>
                Kind
              </th>
              <th className={`${th} text-right`} {...help("rows")}>
                Rows
              </th>
              <th className={`${th} text-right`} {...help("columns")}>
                Columns
              </th>
              <th className={th}>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody className={tbody}>
            {group.relations.map((r) => (
              <tr key={r.name}>
                <td className={`${td} font-medium`}>
                  <span className="flex flex-wrap items-center gap-2">
                    <Link to={{ name: "table", schema: r.schema, table: r.name }} {...help("table")}>
                      {r.name}
                    </Link>
                    {r.writableThrough?.map((a) => <WritableBadge key={a.grant} access={a} />)}
                  </span>
                </td>
                <td className={`${td} text-slate-600 dark:text-slate-300`}>{kindLabel(r.kind)}</td>
                <td className={`${td} text-right tabular-nums text-slate-600 dark:text-slate-300`}>{r.kind === "view" ? "—" : formatApproxRows(r.approxRows)}</td>
                <td className={`${td} text-right tabular-nums text-slate-600 dark:text-slate-300`}>{r.columns.length}</td>
                <td className={`${td} text-right`}>
                  <QueryButton schema={r.schema} table={r.name} onQuery={onQuery} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

/** Opens the docked console with a bounded SELECT of one table. */
function QueryButton({ schema, table, onQuery }: { schema: string; table: string; onQuery: QueryTable }) {
  return (
    <button
      type="button"
      onClick={() => onQuery(schema, table)}
      aria-label={`Query ${schema}.${table} in the console`}
      title="Put a SELECT of its first 100 rows in the SQL console below."
      className="rounded-md border border-slate-300 px-2 py-0.5 text-xs font-medium text-slate-700 hover:bg-slate-100 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
    >
      Query
    </button>
  );
}

/** One table's page: its columns and a preview of its first 100 rows. */
export function TablePage({ api, schema, table, onQuery }: { api: ApiContext; schema: string; table: string; onQuery: QueryTable }) {
  const list = useLoad(() => listTables(api), [api]);
  const preview = useLoad(() => previewTable(api, schema, table), [api, schema, table]);
  const rel = list.state.status === "ready" ? list.state.data.tables.find((r) => r.schema === schema && r.name === table) : undefined;

  return (
    <section aria-labelledby="table-heading" className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="text-xs">
            <Link to={{ name: "tables" }}>Tables</Link>
            <span className="text-slate-400 dark:text-slate-500">
              {" / "}
              <span {...help("schema")}>{schema}</span>
            </span>
          </p>
          <h2 id="table-heading" className="mt-1 break-all font-mono text-lg font-semibold">
            {schema}.{table}
          </h2>
          {rel && (
            <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
              {kindLabel(rel.kind)}
              {rel.kind !== "view" && <> · {formatApproxRows(rel.approxRows)}</>}
              {rel.comment && <> · {rel.comment}</>}
            </p>
          )}
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => onQuery(schema, table)}
            title="Put a SELECT of its first 100 rows in the SQL console below."
            className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            Query below
          </button>
          <Link
            to={{ name: "sql", schema, table }}
            className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            Open in SQL console
          </Link>
        </div>
      </div>

      {list.state.status === "error" && <Alert>Couldn&apos;t load the table: {list.state.error}</Alert>}
      {list.state.status === "ready" && !rel && <Alert>There&apos;s no table called {`${schema}.${table}`} in this workspace&apos;s database.</Alert>}
      {rel?.writableThrough && rel.writableThrough.length > 0 && (
        <div className="flex flex-col gap-1">
          {rel.writableThrough.map((a) => (
            <p key={a.grant} className="flex flex-wrap items-center gap-2 text-sm text-slate-600 dark:text-slate-300">
              <WritableBadge access={a} />
              <span>
                {a.operations.join(", ")}
                {a.columns.length > 0 && <> · columns {a.columns.join(", ")}</>}
              </span>
            </p>
          ))}
        </div>
      )}
      {rel && <ColumnsTable rel={rel} />}

      <div className="flex flex-col gap-2">
        <h3 className="text-sm font-semibold">Preview</h3>
        <p className="text-xs text-slate-500 dark:text-slate-400">The first 100 rows, read-only.</p>
        {preview.state.status === "loading" && <Muted>Loading rows…</Muted>}
        {preview.state.status === "error" && <Alert>Couldn&apos;t preview rows: {preview.state.error}</Alert>}
        {preview.state.status === "ready" && (
          <>
            <ResultSummary result={preview.state.data} />
            <ResultGrid result={preview.state.data} />
          </>
        )}
      </div>
    </section>
  );
}

function ColumnsTable({ rel }: { rel: Relation }) {
  return (
    <div className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold">Columns</h3>
      <div className={tableShell}>
        <table className={tableEl}>
          <thead className={thead}>
            <tr>
              <th className={th} {...help("columnName")}>
                Name
              </th>
              <th className={th} {...help("columnType")}>
                Type
              </th>
              <th className={th} {...help("nullable")}>
                Nullable
              </th>
              <th className={th} {...help("comment")}>
                Comment
              </th>
            </tr>
          </thead>
          <tbody className={tbody}>
            {rel.columns.map((c) => (
              <tr key={c.name}>
                <td className={`${td} font-mono`}>{c.name}</td>
                <td className={`${td} font-mono text-slate-600 dark:text-slate-300`}>{c.type}</td>
                <td className={`${td} text-slate-600 dark:text-slate-300`}>{c.nullable ? "Yes" : "No"}</td>
                <td className={`${td} text-slate-500 dark:text-slate-400`}>{c.comment ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
