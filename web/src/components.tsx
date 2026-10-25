import type { MouseEvent, ReactNode } from "react";
import { API_MODULE_PATH, hrefFor, navigate, navigateTo, type Route } from "./navigation";
import type { CellValue, QueryResult, WriteAccess } from "./types";

// Shared pieces. Every class name is a complete static string, and every colour has its dark:
// pair: booth-design builds one stylesheet by scanning this package's compiled JS (ADR 0097).

/** An in-shell link: a real href (so open-in-new-tab and copy-link work), navigated without a
 *  page reload on a plain click. */
export function Link({
  to,
  children,
  className,
  current,
  title,
  "aria-describedby": describedBy,
}: {
  to: Route;
  children: ReactNode;
  className?: string;
  current?: boolean;
  title?: string;
  "aria-describedby"?: string;
}) {
  const onClick = (ev: MouseEvent<HTMLAnchorElement>) => {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    ev.preventDefault();
    navigate(to);
  };
  return (
    <a
      href={hrefFor(to)}
      onClick={onClick}
      aria-current={current ? "page" : undefined}
      title={title}
      aria-describedby={describedBy}
      className={className ?? "text-indigo-600 hover:underline dark:text-indigo-400"}
    >
      {children}
    </a>
  );
}

export function Panel({ children }: { children: ReactNode }) {
  return <div className="rounded-lg border border-slate-200 bg-white p-4 dark:border-slate-700 dark:bg-slate-900">{children}</div>;
}

export function Muted({ children }: { children: ReactNode }) {
  return <p className="text-sm text-slate-500 dark:text-slate-400">{children}</p>;
}

export function Alert({ children }: { children: ReactNode }) {
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
      {children}
    </div>
  );
}

export function Notice({ children }: { children: ReactNode }) {
  return (
    <div role="status" className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:border-amber-900 dark:bg-amber-950 dark:text-amber-200">
      {children}
    </div>
  );
}

export const th = "whitespace-nowrap px-3 py-2 text-left text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400";
export const td = "px-3 py-2 text-sm";
export const tableShell = "overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-700";
export const tableEl = "min-w-full divide-y divide-slate-200 dark:divide-slate-700";
export const thead = "bg-slate-50 dark:bg-slate-800/60";
export const tbody = "divide-y divide-slate-100 bg-white dark:divide-slate-800 dark:bg-slate-900";

function Cell({ value }: { value: CellValue }) {
  if (value === null) return <span className="italic text-slate-400 dark:text-slate-500">NULL</span>;
  if (typeof value === "boolean") return <span className="font-mono">{value ? "true" : "false"}</span>;
  if (typeof value === "number") return <span className="font-mono tabular-nums">{value}</span>;
  return <span className="whitespace-pre font-mono">{value.length > 300 ? value.slice(0, 300) + "…" : value}</span>;
}

/** The rows of a preview or console query, values shown as the server rendered them. */
export function ResultGrid({ result }: { result: QueryResult }) {
  if (result.columns.length === 0) {
    return <Muted>{result.commandTag ? `Done: ${result.commandTag}` : "The statement returned no columns."}</Muted>;
  }
  return (
    <div className={`${tableShell} max-h-[32rem] overflow-y-auto`}>
      <table className={tableEl}>
        <thead className={`${thead} sticky top-0`}>
          <tr>
            {result.columns.map((c, i) => (
              <th key={i} className={th} title={c.type}>
                {c.name}
                {c.type && <span className="ml-1 font-normal normal-case text-slate-400 dark:text-slate-500">{c.type}</span>}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className={tbody}>
          {result.rows.map((row, r) => (
            <tr key={r}>
              {row.map((v, c) => (
                <td key={c} className={`${td} align-top text-slate-800 dark:text-slate-200`}>
                  <Cell value={v} />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** "12 rows in 34 ms" plus, when the server cut the result, why. */
export function ResultSummary({ result }: { result: QueryResult }) {
  const rows = `${result.rowCount.toLocaleString("en-US")} ${result.rowCount === 1 ? "row" : "rows"} in ${result.durationMs} ms`;
  return (
    <div className="flex flex-col gap-2">
      <p className="text-xs text-slate-500 dark:text-slate-400">{rows}</p>
      {result.truncated && <Notice>{truncatedMessage(result)}</Notice>}
    </div>
  );
}

export function truncatedMessage(result: QueryResult): string {
  switch (result.truncatedReason) {
    case "rows":
      return `Truncated: only the first ${result.rowCount.toLocaleString("en-US")} rows are shown. Add a LIMIT or a WHERE clause to see the rest.`;
    case "bytes":
      return "Truncated: the result reached the 5 MiB display limit. Select fewer or narrower columns.";
    default:
      return "Some long values were shortened for display.";
  }
}

/** ADR 0117's "Writable through an API" badge: links to the API page, naming the API when the
 *  grant id says which. Read-only: writes are turned off on the API page. */
export function WritableBadge({ access }: { access: WriteAccess }) {
  const label = access.api ? `Writable through API ${access.api}` : "Writable through an API";
  const detail = `${access.operations.join(", ")}${access.columns.length ? ` · columns: ${access.columns.join(", ")}` : ""}`;
  const onClick = (ev: MouseEvent<HTMLAnchorElement>) => {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    ev.preventDefault();
    navigateTo(API_MODULE_PATH);
  };
  return (
    <a
      href={API_MODULE_PATH}
      onClick={onClick}
      title={detail}
      className="inline-flex items-center rounded-full border border-amber-300 bg-amber-50 px-2 py-0.5 text-xs font-medium text-amber-800 hover:underline dark:border-amber-700 dark:bg-amber-950 dark:text-amber-300"
    >
      {label}
    </a>
  );
}
