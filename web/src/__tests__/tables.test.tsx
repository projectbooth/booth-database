import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { prefillFor, quoteIdent } from "../ConsoleView";
import { DatabaseApp } from "../DatabaseApp";
import { formatApproxRows } from "../format";
import { countLabel, groupByProject, groupBySchema } from "../grouping";
import { parseRoute, routePath, type Route } from "../navigation";
import type { QueryResult, Relation, WorkspaceRole } from "../types";

interface Call {
  method: string;
  path: string;
  search: string;
  body?: string;
}

type Reply = { status?: number; json?: unknown } | Promise<{ status?: number; json?: unknown }>;

function mockFetch(routes: Record<string, (call: Call) => Reply>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://localhost");
      const call: Call = { method: init?.method ?? "GET", path: url.pathname, search: url.search, body: init?.body as string | undefined };
      calls.push(call);
      const route = routes[`${call.method} ${call.path}`];
      const reply = route ? await route(call) : { status: 404, json: { error: "not routed" } };
      return new Response(reply.status === 204 ? null : JSON.stringify(reply.json ?? {}), { status: reply.status ?? 200 });
    }),
  );
  return calls;
}

function at(path: string) {
  window.history.pushState({}, "", path);
}

function mount(role: WorkspaceRole = "viewer") {
  return render(<DatabaseApp workspace="acme" role={role} theme="dark" getAccessToken={() => "tok"} />);
}

const PLAYERS: Relation = {
  schema: "nfl_data",
  name: "players",
  kind: "table",
  comment: "One row per player",
  columns: [
    { name: "id", type: "integer", nullable: false, comment: null },
    { name: "name", type: "text", nullable: true, comment: "Full name" },
  ],
  approxRows: 12345,
  schemaHash: "h1",
};
const VIEW: Relation = { schema: "public", name: "recent", kind: "view", comment: null, columns: [{ name: "id", type: "integer", nullable: true, comment: null }], approxRows: null, schemaHash: "h2" };

const RESULT: QueryResult = {
  columns: [
    { name: "id", type: "integer" },
    { name: "name", type: "text" },
  ],
  rows: [
    [1, "Ann"],
    [2, null],
  ],
  rowCount: 2,
  truncated: false,
  commandTag: "SELECT 2",
  durationMs: 7,
};

beforeEach(() => at("/database"));
afterEach(() => vi.unstubAllGlobals());

describe("deep links (the Catalog links to these: keep them stable)", () => {
  const cases: [Route, string][] = [
    [{ name: "overview" }, "/database"],
    [{ name: "tables" }, "/database/tables"],
    [{ name: "table", schema: "nfl_data", table: "players" }, "/database/tables/nfl_data/players"],
    [{ name: "table", schema: "My Schema", table: 'a/b"c' }, "/database/tables/My%20Schema/a%2Fb%22c"],
    [{ name: "sql" }, "/database/sql"],
    [{ name: "sql", schema: "nfl_data", table: "players" }, "/database/sql?schema=nfl_data&name=players"],
  ];
  it.each(cases)("%j <-> %s", (route, path) => {
    expect(routePath(route)).toBe(path);
    const [p, q] = path.split("?");
    expect(parseRoute(p, q ? `?${q}` : "")).toEqual(route);
  });

  it("sends anything unknown to a useful place", () => {
    expect(parseRoute("/database/nope", "")).toEqual({ name: "overview" });
    expect(parseRoute("/database/tables/only-one", "")).toEqual({ name: "tables" });
    expect(parseRoute("/database/tables/%E0%A4%A/x", "")).toEqual({ name: "tables" });
    expect(parseRoute("/elsewhere", "")).toEqual({ name: "overview" });
  });
});

describe("Tables tab", () => {
  it("lists tables with schema, kind and an 'about' row count, and searches names and columns", async () => {
    at("/database/tables");
    const calls = mockFetch({ "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS, VIEW] } }) });
    mount();
    expect(await screen.findByRole("link", { name: "players" })).toHaveAttribute("href", "/database/tables/nfl_data/players");
    expect(screen.getByText("about 12,000 rows")).toBeInTheDocument();
    expect(screen.getByText("View")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Tables" })).toHaveAttribute("aria-current", "page");
    expect(calls[0].path).toBe("/modules/database/api/tables");

    await userEvent.type(screen.getByRole("searchbox"), "full");
    expect(screen.queryByRole("link", { name: "players" })).not.toBeInTheDocument();
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "name");
    expect(screen.getByRole("link", { name: "players" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "recent" })).not.toBeInTheDocument();
  });

  it("explains a workspace with no database", async () => {
    at("/database/tables");
    mockFetch({ "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: false, tables: [] } }) });
    mount();
    expect(await screen.findByText("No database yet")).toBeInTheDocument();
  });

  it("navigates to a table page without a reload", async () => {
    at("/database/tables");
    mockFetch({
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS] } }),
      "GET /modules/database/api/tables/preview": () => ({ json: RESULT }),
    });
    mount();
    await userEvent.click(await screen.findByRole("link", { name: "players" }));
    expect(window.location.pathname).toBe("/database/tables/nfl_data/players");
    expect(await screen.findByRole("heading", { name: "nfl_data.players" })).toBeInTheDocument();
  });
});

describe("schema groups", () => {
  const GAMES: Relation = { ...PLAYERS, name: "games", schemaHash: "h3" };
  const LEDGER: Relation = { ...PLAYERS, schema: "finance_dev", name: "ledger", schemaHash: "h4" };
  const STATE: Relation = { ...PLAYERS, schema: "booth_database", name: "state", schemaHash: "h5" };
  const tablesReply = (tables: Relation[]) => ({ "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables } }) });
  const schemaHeadings = () => screen.getAllByRole("button", { name: /table|view/ }).filter((b) => b.hasAttribute("aria-expanded"));

  it("groups by schema with counts, the workspace's schemas first and system schemas last and collapsed", async () => {
    at("/database/tables");
    mockFetch(tablesReply([STATE, LEDGER, PLAYERS, GAMES, VIEW]));
    mount();
    await screen.findByRole("link", { name: "players" });
    expect(schemaHeadings().map((b) => [b.textContent, b.getAttribute("aria-expanded")])).toEqual([
      [expect.stringMatching(/finance_dev1 table$/), "true"],
      [expect.stringMatching(/nfl_data2 tables$/), "true"],
      [expect.stringMatching(/public1 view$/), "true"],
      [expect.stringMatching(/booth_database1 table· system$/), "false"],
    ]);
    expect(screen.queryByRole("link", { name: "state" })).toBeNull();
    const nfl = screen.getByRole("region", { name: "Schema nfl_data" });
    expect(within(nfl).getAllByRole("link").map((a) => a.textContent)).toEqual(["players", "games"]);
  });

  it("collapses and expands a schema, and a search finds matches in collapsed schemas", async () => {
    at("/database/tables");
    mockFetch(tablesReply([LEDGER, PLAYERS, STATE]));
    mount();
    await screen.findByRole("link", { name: "players" });
    const nflHeading = screen.getByRole("button", { name: /^nfl_data/ });
    await userEvent.click(nflHeading);
    expect(nflHeading).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("link", { name: "players" })).toBeNull();
    expect(screen.getByRole("link", { name: "ledger" })).toBeInTheDocument();

    await userEvent.type(screen.getByRole("searchbox"), "sta");
    expect(screen.getByRole("link", { name: "state" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Schema finance_dev" })).toBeNull();
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "players");
    expect(screen.getByRole("link", { name: "players" })).toBeInTheDocument();
    await userEvent.clear(screen.getByRole("searchbox"));
    expect(screen.queryByRole("link", { name: "players" })).toBeNull();
    expect(screen.getByRole("button", { name: /^nfl_data/ })).toHaveAttribute("aria-expanded", "false");

    await userEvent.click(screen.getByRole("button", { name: /^nfl_data/ }));
    expect(screen.getByRole("link", { name: "players" })).toBeInTheDocument();
  });

  it("has no project logic yet: one group, no project heading", () => {
    const schemas = groupBySchema([PLAYERS, LEDGER]);
    expect(groupByProject(schemas)).toEqual([{ project: null, schemas }]);
    expect(countLabel([PLAYERS, VIEW, { ...VIEW, kind: "materialized_view" }])).toBe("1 table, 2 views");
    expect(countLabel([])).toBe("0 tables");
  });
});

describe("docked SQL console on the Tables tab", () => {
  beforeEach(() => window.localStorage.clear());
  const PREFILL = 'SELECT *\nFROM "nfl_data"."players"\nLIMIT 100';

  it("starts closed, opens prefilled from a table's Query button, runs, and keeps its result on a table page", async () => {
    at("/database/tables");
    const calls = mockFetch({
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS] } }),
      "GET /modules/database/api/tables/preview": () => ({ json: { ...RESULT, rows: [[9, "Preview row"]] } }),
      "POST /modules/database/api/query": () => ({ json: RESULT }),
    });
    mount();
    await screen.findByRole("link", { name: "players" });
    const toggle = screen.getByRole("button", { name: /^SQL console/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("textbox", { name: "SQL statement" })).toBeNull();

    await userEvent.click(screen.getByRole("button", { name: "Query nfl_data.players in the console" }));
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const editor = screen.getByRole("textbox", { name: "SQL statement" });
    expect(editor).toHaveValue(PREFILL);
    expect(editor).toHaveFocus();
    expect(screen.getByText(/stopped after 30 seconds, and showing at most 1,000 rows/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByText("Ann")).toBeInTheDocument();
    expect(JSON.parse(calls.find((c) => c.method === "POST")!.body!)).toEqual({ sql: PREFILL });
    expect(window.localStorage.getItem("booth-database:console-dock-open")).toBe("1");

    await userEvent.click(screen.getByRole("link", { name: "players" }));
    expect(await screen.findByText("Preview row")).toBeInTheDocument();
    expect(screen.getByText("Ann")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /^SQL console/ }));
    expect(screen.queryByRole("textbox", { name: "SQL statement" })).toBeNull();
    expect(window.localStorage.getItem("booth-database:console-dock-open")).toBe("0");
  });

  it("prefills from a table page and remembers being open; the SQL console tab has no dock", async () => {
    window.localStorage.setItem("booth-database:console-dock-open", "1");
    at("/database/tables/nfl_data/players");
    mockFetch({
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS] } }),
      "GET /modules/database/api/tables/preview": () => ({ json: RESULT }),
    });
    mount();
    const editor = await screen.findByRole("textbox", { name: "SQL statement" });
    expect(editor).toHaveValue("");
    await userEvent.click(screen.getByRole("button", { name: "Query below" }));
    expect(editor).toHaveValue(PREFILL);

    await userEvent.click(within(screen.getByRole("navigation", { name: "Database sections" })).getByRole("link", { name: "SQL console" }));
    expect(screen.queryByRole("complementary", { name: "SQL console" })).toBeNull();
    expect(screen.getAllByRole("textbox", { name: "SQL statement" })).toHaveLength(1);
  });
});

describe("help text", () => {
  it("describes column headers, schema and table labels", async () => {
    at("/database/tables");
    mockFetch({
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS] } }),
      "GET /modules/database/api/tables/preview": () => ({ json: RESULT }),
    });
    mount();
    const link = await screen.findByRole("link", { name: "players" });
    expect(link).toHaveAccessibleDescription(/columns and a preview/);
    expect(link).toHaveAttribute("title", expect.stringMatching(/columns and a preview/));
    expect(screen.getByRole("columnheader", { name: "Rows" })).toHaveAccessibleDescription(/estimate/);
    expect(screen.getByRole("columnheader", { name: "Kind" })).toHaveAttribute("title", expect.stringMatching(/^Table: stored rows/));
    expect(screen.getByText("nfl_data")).toHaveAccessibleDescription(/named group of tables/);

    await userEvent.click(link);
    expect(await screen.findByRole("columnheader", { name: "Nullable" })).toHaveAccessibleDescription(/left empty/);
    expect(screen.getByRole("columnheader", { name: "Type" })).toHaveAccessibleDescription(/kind of value/);
    expect(screen.getByText("nfl_data", { selector: "span" })).toHaveAccessibleDescription(/named group of tables/);
  });
});

describe("Writable through an API (ADR 0117)", () => {
  const GRANTED: Relation = {
    ...PLAYERS,
    writableThrough: [{ grant: "api:finance-api", api: "finance-api", operations: ["insert", "update"], columns: ["name"] }],
  };
  const OTHER: Relation = { ...VIEW, writableThrough: [{ grant: "manual-grant", operations: ["delete"], columns: [] }] };

  it("badges writable tables, names the API, and links to the API page without a reload", async () => {
    mockFetch({ "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [GRANTED, OTHER] } }) });
    at("/database/tables");
    mount();
    const badge = await screen.findByRole("link", { name: "Writable through API finance-api" });
    expect(badge).toHaveAttribute("href", "/apis");
    expect(screen.getByRole("link", { name: "Writable through an API" })).toBeInTheDocument();
    const pop = vi.fn();
    window.addEventListener("popstate", pop);
    await userEvent.click(badge);
    window.removeEventListener("popstate", pop);
    expect(window.location.pathname).toBe("/apis");
    expect(pop).toHaveBeenCalled();
  });

  it("shows the grant's operations and columns on the table page, and no revoke control", async () => {
    mockFetch({
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [GRANTED] } }),
      "GET /modules/database/api/tables/preview": () => ({ json: RESULT }),
    });
    at("/database/tables/nfl_data/players");
    mount("owner");
    expect(await screen.findByRole("link", { name: "Writable through API finance-api" })).toBeInTheDocument();
    expect(screen.getByText(/insert, update/)).toHaveTextContent("insert, update · columns name");
    expect(screen.queryByRole("button", { name: /revoke/i })).toBeNull();
  });

  it("shows no badge on tables nothing can write", async () => {
    mockFetch({ "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS] } }) });
    at("/database/tables");
    mount();
    await screen.findByRole("link", { name: "players" });
    expect(screen.queryByText(/Writable through/)).toBeNull();
  });
});

describe("table page", () => {
  it("shows columns and a preview, and links to the console prefilled", async () => {
    at("/database/tables/nfl_data/players");
    const calls = mockFetch({
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS] } }),
      "GET /modules/database/api/tables/preview": () => ({ json: RESULT }),
    });
    mount();
    expect(await screen.findByText("Full name")).toBeInTheDocument();
    expect(await screen.findByText("Ann")).toBeInTheDocument();
    expect(screen.getByText("NULL")).toBeInTheDocument();
    expect(screen.getByText("2 rows in 7 ms")).toBeInTheDocument();
    expect(screen.getByText(/about 12,000 rows/)).toBeInTheDocument();
    const preview = calls.find((c) => c.path.endsWith("/preview"))!;
    expect(new URLSearchParams(preview.search).get("schema")).toBe("nfl_data");
    expect(new URLSearchParams(preview.search).get("name")).toBe("players");
    expect(screen.getByRole("link", { name: "Open in SQL console" })).toHaveAttribute("href", "/database/sql?schema=nfl_data&name=players");
  });

  it("says when the table doesn't exist", async () => {
    at("/database/tables/public/gone");
    mockFetch({
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [PLAYERS] } }),
      "GET /modules/database/api/tables/preview": () => ({ status: 404, json: { error: 'relation "public.gone" does not exist', code: "not_found" } }),
    });
    mount();
    expect(await screen.findByText(/no table called public.gone/)).toBeInTheDocument();
  });
});

describe("SQL console", () => {
  it("prefills from a deep link, runs on click, and shows the result", async () => {
    at("/database/sql?schema=nfl_data&name=players");
    const calls = mockFetch({ "POST /modules/database/api/query": () => ({ json: RESULT }) });
    mount();
    const editor = screen.getByRole("textbox", { name: "SQL statement" });
    expect(editor).toHaveValue('SELECT *\nFROM "nfl_data"."players"\nLIMIT 100');
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByText("Ann")).toBeInTheDocument();
    expect(JSON.parse(calls[0].body!)).toEqual({ sql: 'SELECT *\nFROM "nfl_data"."players"\nLIMIT 100' });
  });

  it("shows the truncated notice", async () => {
    at("/database/sql");
    mockFetch({ "POST /modules/database/api/query": () => ({ json: { ...RESULT, truncated: true, truncatedReason: "rows", rowCount: 1000 } }) });
    mount();
    await userEvent.type(screen.getByRole("textbox", { name: "SQL statement" }), "SELECT 1");
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByRole("status")).toHaveTextContent(/Truncated: only the first 1,000 rows/);
  });

  it("shows the server's error text", async () => {
    at("/database/sql");
    mockFetch({
      "POST /modules/database/api/query": () => ({ status: 422, json: { error: "the SQL console is read-only: cannot execute INSERT in a read-only transaction", code: "read_only" } }),
    });
    mount("owner");
    await userEvent.type(screen.getByRole("textbox", { name: "SQL statement" }), "INSERT INTO t VALUES (1)");
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("cannot execute INSERT in a read-only transaction");
  });

  it("cancels a running query", async () => {
    at("/database/sql");
    let finish: (r: { status: number; json: unknown }) => void = () => undefined;
    const calls = mockFetch({
      "POST /modules/database/api/query": () => new Promise((resolve) => (finish = resolve)),
      "DELETE /modules/database/api/query": () => {
        finish({ status: 422, json: { error: "the query was cancelled", code: "cancelled" } });
        return { status: 204 };
      },
    });
    mount();
    await userEvent.type(screen.getByRole("textbox", { name: "SQL statement" }), "SELECT pg_sleep(60)");
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByText("Running…")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The query was cancelled.");
    expect(calls.map((c) => c.method)).toEqual(["POST", "DELETE"]);
    expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
  });

  it("runs on Ctrl+Enter", async () => {
    at("/database/sql");
    const calls = mockFetch({ "POST /modules/database/api/query": () => ({ json: RESULT }) });
    mount();
    const editor = screen.getByRole("textbox", { name: "SQL statement" });
    await userEvent.type(editor, "SELECT 1{Control>}{Enter}{/Control}");
    await waitFor(() => expect(calls).toHaveLength(1));
  });

  it("quotes identifiers in the prefill", () => {
    expect(quoteIdent('a"b')).toBe('"a""b"');
    expect(prefillFor('s"', "t")).toBe('SELECT *\nFROM "s"""."t"\nLIMIT 100');
    expect(prefillFor()).toBe("");
  });
});

describe("tabs", () => {
  it("switches between overview, tables and console", async () => {
    mockFetch({
      "GET /modules/database/api/databases": () => ({ status: 403, json: { error: "no" } }),
      "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables: [] } }),
    });
    mount();
    const nav = screen.getByRole("navigation", { name: "Database sections" });
    expect(within(nav).getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");
    await userEvent.click(within(nav).getByRole("link", { name: "Tables" }));
    expect(await screen.findByText(/no tables yet/)).toBeInTheDocument();
    await userEvent.click(within(nav).getByRole("link", { name: "SQL console" }));
    expect(screen.getByRole("textbox", { name: "SQL statement" })).toBeInTheDocument();
  });
});

describe("formatApproxRows", () => {
  it("always says about, and unknown for no estimate", () => {
    expect(formatApproxRows(null)).toBe("Unknown");
    expect(formatApproxRows(1)).toBe("about 1 row");
    expect(formatApproxRows(500)).toBe("about 500 rows");
    expect(formatApproxRows(12345)).toBe("about 12,000 rows");
    expect(formatApproxRows(9_876_543)).toBe("about 9,900,000 rows");
  });
});
