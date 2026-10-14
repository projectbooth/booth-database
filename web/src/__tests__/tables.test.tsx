import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { prefillFor, quoteIdent } from "../ConsoleView";
import { DatabaseApp } from "../DatabaseApp";
import { formatApproxRows } from "../format";
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
