import { useMemo, useState } from "react";
import { listTables, previewTable, type ApiContext } from "./api/client";
import { Alert, Link, Muted, Panel, ResultGrid, ResultSummary, tableEl, tableShell, tbody, td, th, thead } from "./components";
import { formatApproxRows, kindLabel } from "./format";
import { useLoad, type LoadState } from "./hooks";
import type { Relation, TablesResponse } from "./types";

/** The Tables tab (ADR 0116): every table, view and materialized view in the workspace database,
 *  for any member. Opening it also asks the server to check for changes for the Catalog. */
export function TablesView({ api }: { api: ApiContext }) {
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
      <TablesBody state={state} q={q} />
    </section>
  );
}

function matches(r: Relation, q: string): boolean {
  if (!q) return true;
  const needle = q.toLowerCase();
  return `${r.schema}.${r.name}`.toLowerCase().includes(needle) || r.columns.some((c) => c.name.toLowerCase().includes(needle));
}

function TablesBody({ state, q }: { state: LoadState<TablesResponse>; q: string }) {
  const filtered = useMemo(() => (state.status === "ready" ? state.data.tables.filter((r) => matches(r, q.trim())) : []), [state, q]);
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
  return (
    <>
      <div className={tableShell}>
        <table className={tableEl}>
          <thead className={thead}>
            <tr>
              <th className={th}>Schema</th>
              <th className={th}>Name</th>
              <th className={th}>Kind</th>
              <th className={`${th} text-right`}>Rows</th>
              <th className={`${th} text-right`}>Columns</th>
            </tr>
          </thead>
          <tbody className={tbody}>
            {filtered.map((r) => (
              <tr key={`${r.schema}\u0000${r.name}`}>
                <td className={`${td} text-slate-500 dark:text-slate-400`}>{r.schema}</td>
                <td className={`${td} font-medium`}>
                  <Link to={{ name: "table", schema: r.schema, table: r.name }}>{r.name}</Link>
                </td>
                <td className={`${td} text-slate-600 dark:text-slate-300`}>{kindLabel(r.kind)}</td>
                <td className={`${td} text-right tabular-nums text-slate-600 dark:text-slate-300`}>{r.kind === "view" ? "—" : formatApproxRows(r.approxRows)}</td>
                <td className={`${td} text-right tabular-nums text-slate-600 dark:text-slate-300`}>{r.columns.length}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {filtered.length === 0 && <Muted>Nothing matches &quot;{q}&quot;.</Muted>}
      <p className="text-xs text-slate-500 dark:text-slate-400">
        Row counts are PostgreSQL&apos;s estimates and can lag behind recent changes.
      </p>
    </>
  );
}

/** One table's page: its columns and a preview of its first 100 rows. */
export function TablePage({ api, schema, table }: { api: ApiContext; schema: string; table: string }) {
  const list = useLoad(() => listTables(api), [api]);
  const preview = useLoad(() => previewTable(api, schema, table), [api, schema, table]);
  const rel = list.state.status === "ready" ? list.state.data.tables.find((r) => r.schema === schema && r.name === table) : undefined;

  return (
    <section aria-labelledby="table-heading" className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="text-xs">
            <Link to={{ name: "tables" }}>Tables</Link>
            <span className="text-slate-400 dark:text-slate-500"> / {schema}</span>
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
        <Link
          to={{ name: "sql", schema, table }}
          className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
        >
          Open in SQL console
        </Link>
      </div>

      {list.state.status === "error" && <Alert>Couldn&apos;t load the table: {list.state.error}</Alert>}
      {list.state.status === "ready" && !rel && <Alert>There&apos;s no table called {`${schema}.${table}`} in this workspace&apos;s database.</Alert>}
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
              <th className={th}>Name</th>
              <th className={th}>Type</th>
              <th className={th}>Nullable</th>
              <th className={th}>Comment</th>
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
