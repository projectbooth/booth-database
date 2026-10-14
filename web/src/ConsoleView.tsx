import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { ApiError, cancelQuery, runQuery, type ApiContext } from "./api/client";
import { Alert, ResultGrid, ResultSummary } from "./components";
import type { QueryResult } from "./types";

/** Quotes one identifier the way PostgreSQL does, for the prefilled statement. */
export function quoteIdent(s: string): string {
  return `"${s.replace(/"/g, '""')}"`;
}

export function prefillFor(schema?: string, table?: string): string {
  return schema && table ? `SELECT *\nFROM ${quoteIdent(schema)}.${quoteIdent(table)}\nLIMIT 100` : "";
}

type RunState = { status: "idle" } | { status: "running" } | { status: "done"; result: QueryResult } | { status: "error"; message: string };

/** The read-only SQL console (ADR 0116). The server enforces every limit described here; the
 *  page only explains them. */
export function ConsoleView({ api, schema, table }: { api: ApiContext; schema?: string; table?: string }) {
  const [sql, setSql] = useState(() => prefillFor(schema, table));
  const [run, setRun] = useState<RunState>({ status: "idle" });
  const [cancelling, setCancelling] = useState(false);
  const generation = useRef(0);
  const inFlight = useRef(false);

  // A new deep link (another table's "Open in SQL console") replaces the text.
  useEffect(() => {
    const prefill = prefillFor(schema, table);
    if (prefill) setSql(prefill);
  }, [schema, table]);

  // Unmounting (or switching workspace) cancels a query still running, so it never holds the
  // caller's one running-query slot.
  useEffect(() => {
    return () => {
      if (inFlight.current) void cancelQuery(api).catch(() => undefined);
      inFlight.current = false;
      generation.current++; // a late answer for the old workspace is ignored
      setRun({ status: "idle" });
    };
  }, [api]);

  const running = run.status === "running";

  const start = async () => {
    if (running || !sql.trim()) return;
    const mine = ++generation.current;
    inFlight.current = true;
    setRun({ status: "running" });
    try {
      const result = await runQuery(api, sql);
      if (generation.current === mine) setRun({ status: "done", result });
    } catch (err) {
      if (generation.current === mine) setRun({ status: "error", message: errorText(err) });
    } finally {
      if (generation.current === mine) inFlight.current = false;
      setCancelling(false);
    }
  };

  const cancel = async () => {
    setCancelling(true);
    try {
      await cancelQuery(api);
    } catch {
      // Nothing was running any more: the query's own response settles the state.
    }
  };

  const onKeyDown = (ev: KeyboardEvent<HTMLTextAreaElement>) => {
    if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) {
      ev.preventDefault();
      void start();
    }
  };

  return (
    <section aria-labelledby="sql-heading" className="flex flex-col gap-3">
      <div>
        <h2 id="sql-heading" className="text-sm font-semibold">
          SQL console
        </h2>
        <p className="mt-1 text-xs text-slate-500 dark:text-slate-400">
          Read-only: one statement at a time, run with viewer access whatever your role, stopped after 30 seconds, and showing at most 1,000 rows. Ctrl+Enter runs.
        </p>
      </div>
      <textarea
        value={sql}
        onChange={(ev) => setSql(ev.target.value)}
        onKeyDown={onKeyDown}
        spellCheck={false}
        rows={8}
        aria-label="SQL statement"
        placeholder="SELECT * FROM my_schema.my_table LIMIT 100"
        className="w-full rounded-md border border-slate-300 bg-white p-3 font-mono text-sm text-slate-900 placeholder:text-slate-400 focus:outline-none focus:ring-2 focus:ring-indigo-500 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-100 dark:placeholder:text-slate-500"
      />
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => void start()}
          disabled={running || !sql.trim()}
          className="rounded-md bg-indigo-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500 disabled:cursor-not-allowed disabled:opacity-50 dark:bg-indigo-500 dark:hover:bg-indigo-400"
        >
          Run
        </button>
        {running && (
          <button
            type="button"
            onClick={() => void cancel()}
            disabled={cancelling}
            className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 focus:outline-none focus:ring-2 focus:ring-indigo-500 disabled:opacity-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            {cancelling ? "Cancelling…" : "Cancel"}
          </button>
        )}
        {running && <span className="text-sm text-slate-500 dark:text-slate-400">Running…</span>}
      </div>
      {run.status === "error" && <Alert>{run.message}</Alert>}
      {run.status === "done" && (
        <>
          <ResultSummary result={run.result} />
          <ResultGrid result={run.result} />
        </>
      )}
    </section>
  );
}

function errorText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.code === "cancelled") return "The query was cancelled.";
    if (err.status === 503) return "The SQL console isn't available: the module has no identity provider configured.";
    return err.message;
  }
  return err instanceof Error ? err.message : String(err);
}
