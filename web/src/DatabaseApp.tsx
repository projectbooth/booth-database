import { useMemo, type ReactNode } from "react";
import { getStatus, listDatabases, type ApiContext, type GetAccessToken } from "./api/client";
import { formatBytes, formatCreated } from "./format";
import { useLoad, type LoadState } from "./hooks";
import type { DatabaseStatus, ListedDatabase, WorkspaceRole } from "./types";

/**
 * Props contract pinned by ADR 0031 (workspace/role/theme) and ADR 0033 (getAccessToken): plain
 * React props, nothing imported from booth-design.
 */
export interface DatabaseAppProps {
  /** Active workspace slug (ADR 0025). */
  workspace: string;
  /** Caller's role. Only owners see status; the backend enforces the same rule on every request
   *  from the token itself (ADR 0041), so this only decides what to *offer*. */
  role: WorkspaceRole;
  theme: "dark" | "light";
  /** booth-design's current bearer token, or null. Called fresh before every request (ADR 0033). */
  getAccessToken: GetAccessToken;
}

/**
 * The native view booth-design mounts for booth-database (ADR 0030, ADR 0093): a read-only status
 * page. Owners see their workspace's database; platform operators (/platform/operator, ADR 0094)
 * see every workspace's database, whatever their role in the workspace they're acting in. Nothing here can create, change or drop a database — deliberately
 * (ADR 0093, ADR 0089). No outer padding: the shell owns it (ADR 0072).
 */
export function DatabaseApp({ workspace, role, theme, getAccessToken }: DatabaseAppProps) {
  const api = useMemo<ApiContext>(() => ({ workspace, getAccessToken }), [workspace, getAccessToken]);
  const isOwner = role === "owner";
  const { state, reload } = useLoad(() => getStatus(api), [api], isOwner);

  return (
    <div data-theme={theme} className="flex flex-col gap-6 text-slate-900 dark:text-slate-100">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold">Database</h1>
          <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
            This workspace&apos;s own PostgreSQL database, used from code with short-lived credentials. Read-only status.
          </p>
        </div>
        {isOwner && (
          <button
            type="button"
            onClick={reload}
            className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            Refresh
          </button>
        )}
      </header>

      {isOwner ? <OwnerView state={state} api={api} /> : <NonOwnerView api={api} />}

      <ConnectSnippet />
    </div>
  );
}

function OwnerView({ state, api }: { state: LoadState<Awaited<ReturnType<typeof getStatus>>>; api: ApiContext }) {
  if (state.status === "loading") return <Muted>Loading database status…</Muted>;
  if (state.status === "error") return <ErrorBanner state={state} />;
  const s = state.data;
  return (
    <>
      {s.provisioned && s.database ? (
        <WorkspaceCard db={s.database} />
      ) : (
        <Panel>
          <p className="text-sm font-medium">No database yet</p>
          <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
            It&apos;s created automatically the first time code in this workspace connects.
          </p>
        </Panel>
      )}
      {s.operator && <AllDatabases api={api} />}
    </>
  );
}

function WorkspaceCard({ db }: { db: DatabaseStatus }) {
  return (
    <Panel>
      <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-5">
        <Stat label="Size" value={formatBytes(db.sizeBytes)} />
        <Stat label="Tables" value={db.tables === undefined ? "—" : String(db.tables)} />
        <Stat label="Active credentials" value={String(db.activeCredentials)} />
        <Stat label="Open connections" value={String(db.openConnections)} />
        <Stat label="Created" value={formatCreated(db.createdAt)} />
      </dl>
      <p className="mt-4 font-mono text-xs text-slate-400 dark:text-slate-500">{db.database}</p>
    </Panel>
  );
}

function AllDatabases({ api }: { api: ApiContext }) {
  const { state } = useLoad(() => listDatabases(api), [api]);
  return <AllDatabasesSection state={state} />;
}

/**
 * A non-owner can't see /api/status, but may still be a platform operator (ADR 0094: operator
 * status is independent of workspace role). The props carry no operator flag, so ask the one
 * route that knows: a 200 means operator, a 403 (the usual answer) means not. The server enforces
 * this either way; the probe only decides what to show.
 */
function NonOwnerView({ api }: { api: ApiContext }) {
  const { state } = useLoad(() => listDatabases(api), [api]);
  return (
    <>
      <NotOwner />
      {state.status === "ready" && <AllDatabasesSection state={state} />}
    </>
  );
}

function AllDatabasesSection({ state }: { state: LoadState<ListedDatabase[]> }) {
  return (
    <section aria-labelledby="all-dbs" className="flex flex-col gap-2">
      <h2 id="all-dbs" className="text-sm font-semibold">
        All workspace databases
      </h2>
      <p className="text-xs text-slate-500 dark:text-slate-400">
        Visible because you&apos;re a platform operator. Workspaces you aren&apos;t a member of are shown by database name only.
      </p>
      {state.status === "loading" && <Muted>Loading…</Muted>}
      {state.status === "error" && <ErrorBanner state={state} />}
      {state.status === "ready" && <DatabaseTable rows={state.data} />}
    </section>
  );
}

function DatabaseTable({ rows }: { rows: ListedDatabase[] }) {
  if (rows.length === 0) return <Muted>No workspace has a database yet.</Muted>;
  const total = rows.reduce((n, r) => n + r.sizeBytes, 0);
  const th = "px-3 py-2 text-left text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400";
  const td = "px-3 py-2 text-sm";
  return (
    <div className="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-700">
      <table className="min-w-full divide-y divide-slate-200 dark:divide-slate-700">
        <thead className="bg-slate-50 dark:bg-slate-800/60">
          <tr>
            <th className={th}>Workspace</th>
            <th className={`${th} text-right`}>Size</th>
            <th className={`${th} text-right`}>Credentials</th>
            <th className={`${th} text-right`}>Connections</th>
            <th className={th}>Created</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-100 dark:divide-slate-800">
          {rows.map((r) => (
            <tr key={r.database}>
              <td className={td}>
                {r.workspace ? <span className="font-medium">{r.workspace}</span> : <span className="font-mono text-xs text-slate-500 dark:text-slate-400">{r.database}</span>}
              </td>
              <td className={`${td} text-right tabular-nums`}>{formatBytes(r.sizeBytes)}</td>
              <td className={`${td} text-right tabular-nums`}>{r.activeCredentials}</td>
              <td className={`${td} text-right tabular-nums`}>{r.openConnections}</td>
              <td className={td}>{formatCreated(r.createdAt)}</td>
            </tr>
          ))}
        </tbody>
        <tfoot className="bg-slate-50 dark:bg-slate-800/60">
          <tr>
            <td className={`${td} font-medium`}>
              {rows.length} {rows.length === 1 ? "database" : "databases"}
            </td>
            <td className={`${td} text-right font-medium tabular-nums`}>{formatBytes(total)}</td>
            <td colSpan={3} />
          </tr>
        </tfoot>
      </table>
    </div>
  );
}

function NotOwner() {
  return (
    <Panel>
      <p className="text-sm text-slate-600 dark:text-slate-300">This workspace&apos;s database status is visible to its owners.</p>
    </Panel>
  );
}

// The in-cluster path since ADR 0095: booth-core's credential sidecar sets DATABASE_URL (a
// credential-free loopback proxy) in notebook and pipeline-task pods. Neither image ships this repo's
// booth_database client, so the snippets use only what those images really have: booth-notebooks'
// `booth.database` and pandas in a notebook; psycopg (3) in booth-pipeline's task image.
export const NOTEBOOK_SNIPPET = `import booth.database, pandas as pd

engine = booth.database.engine()
pd.read_sql("SELECT now()", engine)`;

export const PIPELINE_SNIPPET = `import os, psycopg

with psycopg.connect(os.environ["DATABASE_URL"]) as conn:
    conn.execute("SELECT now()").fetchone()`;

const preClass = "overflow-x-auto rounded-lg bg-slate-100 p-3 font-mono text-xs text-slate-800 dark:bg-slate-900 dark:text-slate-200";

function ConnectSnippet() {
  return (
    <section aria-labelledby="connect" className="flex flex-col gap-2">
      <h2 id="connect" className="text-sm font-semibold">
        Connect from a notebook or pipeline task
      </h2>
      <h3 className="text-xs font-medium text-slate-600 dark:text-slate-300">In a notebook</h3>
      <pre data-testid="snippet-notebook" className={preClass}>
        {NOTEBOOK_SNIPPET}
      </pre>
      <h3 className="text-xs font-medium text-slate-600 dark:text-slate-300">In a pipeline task with platform access</h3>
      <pre data-testid="snippet-pipeline" className={preClass}>
        {PIPELINE_SNIPPET}
      </pre>
      <ul className="list-disc space-y-1 pl-4 text-xs text-slate-500 dark:text-slate-400">
        <li>No password to manage: a credential sidecar provides DATABASE_URL, and each connection gets its own credential, valid for about an hour.</li>
        <li>
          A connection ends when the credential it opened with expires, even after renewal, and is guaranteed only about half that (around 30 minutes). Reconnect for longer
          work; booth.database.engine() does this for you between queries. A query still running at expiry is lost.
        </li>
        <li>Access follows your workspace role when the notebook server or task starts: owners and editors can read and write, viewers can only read.</li>
      </ul>
    </section>
  );
}

function ErrorBanner({ state }: { state: { error: string; httpStatus?: number } }) {
  const message =
    state.httpStatus === 503
      ? "Database status isn't available: the module has no identity provider configured."
      : state.httpStatus === 403
        ? "You don't have access to this view in this workspace."
        : `Couldn't load database status: ${state.error}`;
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
      {message}
    </div>
  );
}

function Panel({ children }: { children: ReactNode }) {
  return <div className="rounded-lg border border-slate-200 bg-white p-4 dark:border-slate-700 dark:bg-slate-900">{children}</div>;
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-xs text-slate-500 dark:text-slate-400">{label}</dt>
      <dd className="mt-0.5 text-lg font-semibold tabular-nums">{value}</dd>
    </div>
  );
}

function Muted({ children }: { children: ReactNode }) {
  return <p className="text-sm text-slate-500 dark:text-slate-400">{children}</p>;
}
