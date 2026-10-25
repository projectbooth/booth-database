// Plain-language help on headers and labels. For now a native title (on hover) plus
// aria-describedby (read by screen readers); booth-design's shared tooltip component replaces the
// title when it ships, using the same texts.

export const HELP = {
  project: "A project groups schemas for navigation. It doesn't change who can see or change anything.",
  noProject: "Schemas that aren't bound to a project.",
  schema: "A schema is a named group of tables inside this workspace's database, like a folder.",
  table: "Open this table to see its columns and a preview of its first 100 rows.",
  name: "The table's or view's name inside its schema.",
  kind: "Table: stored rows. View: a saved query over other tables. Materialized view: a saved query whose result is stored.",
  rows: "PostgreSQL's estimate of how many rows are stored. It can lag behind recent changes.",
  columns: "How many columns it has.",
  columnName: "The column's name.",
  columnType: "The kind of value the column holds, in PostgreSQL's terms.",
  nullable: "Whether the column may be left empty (NULL).",
  comment: "The description the table's owner wrote for this column, if any.",
} as const;

export type HelpKey = keyof typeof HELP;

const helpId = (key: HelpKey) => `booth-database-help-${key}`;

/** Props that attach one help text to an element. */
export function help(key: HelpKey): { title: string; "aria-describedby": string } {
  return { title: HELP[key], "aria-describedby": helpId(key) };
}

/** The texts aria-describedby points at. Rendered once, by DatabaseApp. */
export function HelpTexts() {
  return (
    <div hidden>
      {(Object.keys(HELP) as HelpKey[]).map((k) => (
        <span key={k} id={helpId(k)}>
          {HELP[k]}
        </span>
      ))}
    </div>
  );
}
