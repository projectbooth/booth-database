const UNITS = ["B", "KB", "MB", "GB", "TB"];

/** 1536 → "1.5 KB". Binary multiples, one decimal above bytes. */
export function formatBytes(n: number): string {
  let v = n;
  let i = 0;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return i === 0 ? `${v} B` : `${v.toFixed(1)} ${UNITS[i]}`;
}

/** An RFC 3339 time as a readable local date, or "Unknown" (databases provisioned before creation
 *  times were recorded carry none). */
export function formatCreated(iso: string | null): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "Unknown";
  return d.toLocaleString(undefined, { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

/** An approximate row count as the UI must show it (ADR 0116: planner statistics lag, so always
 *  "about"). Null means PostgreSQL has no estimate yet (or it's a view). */
export function formatApproxRows(n: number | null): string {
  if (n === null) return "Unknown";
  const rounded = n < 1000 ? n : Number(n.toPrecision(2));
  return `about ${rounded.toLocaleString("en-US")} ${n === 1 ? "row" : "rows"}`;
}

const KIND_LABELS = { table: "Table", view: "View", materialized_view: "Materialized view" } as const;

export function kindLabel(kind: keyof typeof KIND_LABELS): string {
  return KIND_LABELS[kind] ?? kind;
}
