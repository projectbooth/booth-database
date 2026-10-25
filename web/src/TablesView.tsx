import { useId, useMemo, useState } from "react";
import { listTables, previewTable, type ApiContext } from "./api/client";
import { Alert, Link, Muted, Panel, ResultGrid, ResultSummary, tableEl, tableShell, tbody, td, th, thead, WritableBadge } from "./components";
import { formatApproxRows, kindLabel } from "./format";
import { countLabel, groupByProject, groupBySchema, type SchemaGroup } from "./grouping";
import { help } from "./help";
import { useLoad, type LoadState } from "./hooks";
import type { Relation, TablesResponse } from "./types";

/** Puts a bounded SELECT of one table in the docked console (DatabaseApp owns the dock). */
export type QueryTable = (schema: string, table: string) => void;

/** The Tables tab (ADR 0116): every table, view and materialized view in the workspace database,
 *  grouped by schema, for any member. Opening it also asks the server to check for changes for
 *  the Catalog. */
export function TablesView({ api, onQuery }: { api: ApiContext; onQuery: QueryTable }) {
  const { state, reload } = useLoad(() => listTables(api), [api]);
  const [q, setQ] = useState("");

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
      <TablesBody state={state} q={q} onQuery={onQuery} />
    </section>
  );
}

function matches(r: Relation, q: string): boolean {
  if (!q) return true;
  const needle = q.toLowerCase();
  return `${r.schema}.${r.name}`.toLowerCase().includes(needle) || r.columns.some((c) => c.name.toLowerCase().includes(needle));
}

function TablesBody({ state, q, onQuery }: { state: LoadState<TablesResponse>; q: string; onQuery: QueryTable }) {
  const needle = q.trim();
  const projects = useMemo(
    () => (state.status === "ready" ? groupByProject(groupBySchema(state.data.tables.filter((r) => matches(r, needle)))) : []),
    [state, needle],
  );
  // Schemas the user has toggled from their default (system schemas start collapsed, the rest
  // open). A search shows every match whatever is collapsed, so filtering always works across all
  // schemas.
  const [toggled, setToggled] = useState<ReadonlySet<string>>(() => new Set());
  const toggle = (schema: string) =>
    setToggled((prev) => {
      const next = new Set(prev);
      if (!next.delete(schema)) next.add(schema);
      return next;
    });

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
  const empty = projects.every((p) => p.schemas.length === 0);
  return (
    <>
      {projects.map((p) => (
        <div key={p.project ?? ""} className="flex flex-col gap-3">
          {/* ADR 0124: a project heading goes here once groupByProject returns projects. */}
          {p.schemas.map((g) => (
            <SchemaSection key={g.schema} group={g} open={needle !== "" || g.system === toggled.has(g.schema)} onToggle={() => toggle(g.schema)} onQuery={onQuery} />
          ))}
        </div>
      ))}
      {empty && <Muted>Nothing matches &quot;{q}&quot;.</Muted>}
      <p className="text-xs text-slate-500 dark:text-slate-400">
        Row counts are PostgreSQL&apos;s estimates and can lag behind recent changes.
      </p>
    </>
  );
}

function SchemaSection({ group, open, onToggle, onQuery }: { group: SchemaGroup; open: boolean; onToggle: () => void; onQuery: QueryTable }) {
  const bodyId = useId();
  return (
    <section aria-label={`Schema ${group.schema}`} className="flex flex-col gap-2">
      <h3 className="text-sm">
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
      </h3>
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
