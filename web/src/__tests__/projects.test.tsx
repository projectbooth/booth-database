// ADR 0124: project headings above schemas, against a fake booth-core (contracts/projects.md).

import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DatabaseApp } from "../DatabaseApp";
import { groupByProject, groupBySchema, type ProjectGroup } from "../grouping";
import { loadProjectIndex, PROJECTS_TIMEOUT_MS } from "../hooks";
import type { Project, ProjectBinding, Relation } from "../types";

interface Call {
  path: string;
  search: string;
  headers: Headers;
}

type Reply = { status?: number; json?: unknown; text?: string } | "throw";

/** A fake shell origin: this module's API at /modules/database/api, booth-core's at /api. */
function fakeCore(routes: Record<string, (call: Call) => Reply>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://localhost");
      const call: Call = { path: url.pathname, search: url.search, headers: new Headers(init?.headers) };
      calls.push(call);
      const route = routes[`${init?.method ?? "GET"} ${call.path}`];
      const reply = route ? route(call) : { status: 404, text: "404 page not found" };
      if (reply === "throw") throw new TypeError("Failed to fetch");
      return new Response(reply.text ?? JSON.stringify(reply.json ?? {}), { status: reply.status ?? 200 });
    }),
  );
  return calls;
}

const rel = (schema: string, name: string): Relation => ({
  schema,
  name,
  kind: "table",
  comment: null,
  columns: [{ name: "id", type: "integer", nullable: false, comment: null }],
  approxRows: 10,
  schemaHash: `${schema}.${name}`,
});

const project = (slug: string, name: string, extra: Partial<Project> = {}): Project => ({
  slug,
  name,
  parent: "",
  description: "",
  color: "",
  icon: "",
  archived: false,
  createdBy: "u",
  createdAt: "2026-10-25T00:00:00Z",
  ...extra,
});

let nextId = 0;
const bind = (value: string, projectSlug: string, kind = "postgres_schema"): ProjectBinding => ({
  id: `b${++nextId}`,
  project: projectSlug,
  kind,
  value,
  createdBy: "u",
  createdAt: "2026-10-25T00:00:00Z",
});

// finance > finance-dev, finance-prod, finance-old (archived); nfl (parent null, as the contract
// writes it); legacy (archived). finance owns a schema itself.
const PROJECTS: Project[] = [
  project("nfl", "NFL", { parent: null }),
  project("finance-prod", "Finance prod", { parent: "finance" }),
  project("legacy", "Legacy", { archived: true }),
  project("finance", "Finance"),
  project("finance-old", "Finance old", { parent: "finance", archived: true }),
  project("finance-dev", "Finance dev", { parent: "finance" }),
];
const BINDINGS: ProjectBinding[] = [
  bind("finance_shared", "finance"),
  bind("finance_dev", "finance-dev"),
  bind("finance_prod", "finance-prod"),
  bind("finance_old", "finance-old"),
  bind("nfl_data", "nfl"),
  bind("legacy_s", "legacy"),
  bind("public", "nfl", "iceberg_namespace"), // another kind: ignored
  bind("orphan", "gone"), // a project core no longer lists: unbound
];
const TABLES: Relation[] = [
  rel("finance_dev", "ledger"),
  rel("finance_old", "archive"),
  rel("finance_prod", "ledger_prod"),
  rel("finance_shared", "accounts"),
  rel("legacy_s", "old_thing"),
  rel("nfl_data", "players"),
  rel("orphan", "lost"),
  rel("public", "recent"),
];

const tablesRoute = (tables: Relation[] = TABLES) => ({
  "GET /modules/database/api/tables": () => ({ json: { workspace: "acme", provisioned: true, tables } }),
});
const registry = (projects: Project[] = PROJECTS, bindings: ProjectBinding[] = BINDINGS) => ({
  "GET /api/projects": () => ({ json: projects }),
  "GET /api/projects/bindings": () => ({ json: bindings }),
});

function at(path: string) {
  window.history.pushState({}, "", path);
}

function mount() {
  return render(<DatabaseApp workspace="acme" role="viewer" theme="light" getAccessToken={() => "tok"} />);
}

/** Every collapsible heading button in document order: [its text, expanded]. */
function headings() {
  return screen
    .getAllByRole("button")
    .filter((b) => b.hasAttribute("aria-expanded") && b.closest("aside") === null)
    .map((b) => [b.textContent?.replace(/^[▾▸]/, ""), b.getAttribute("aria-expanded")]);
}

/** A shape of groupByProject's output that's easy to read in a test. */
function outline(groups: ProjectGroup[]): unknown[] {
  return groups.map((g) =>
    g.kind === "project"
      ? { project: g.project.slug, last: g.archivedLast, schemas: g.schemas.map((s) => s.schema), children: outline(g.children) }
      : { [g.kind]: g.schemas.map((s) => s.schema) },
  );
}

beforeEach(() => at("/database/tables"));
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("groupByProject", () => {
  const schemas = groupBySchema(TABLES);

  it("nests sub-projects under their parent, the parent's own schemas first, archived last, No project at the end", () => {
    expect(outline(groupByProject(schemas, { projects: PROJECTS, bindings: BINDINGS }))).toEqual([
      {
        project: "finance",
        last: false,
        schemas: ["finance_shared"],
        children: [
          { project: "finance-dev", last: false, schemas: ["finance_dev"], children: [] },
          { project: "finance-prod", last: false, schemas: ["finance_prod"], children: [] },
          { project: "finance-old", last: true, schemas: ["finance_old"], children: [] },
        ],
      },
      { project: "nfl", last: false, schemas: ["nfl_data"], children: [] },
      { project: "legacy", last: true, schemas: ["legacy_s"], children: [] },
      { unassigned: ["orphan", "public"] },
    ]);
  });

  it("keeps an archived parent with an active sub-project among the active ones", () => {
    const projects = [project("aaa", "Aaa"), project("old", "Old", { archived: true }), project("old-live", "Old live", { parent: "old" })];
    const groups = groupByProject(groupBySchema([rel("a", "t"), rel("ol", "t")]), { projects, bindings: [bind("a", "aaa"), bind("ol", "old-live")] });
    expect(outline(groups)).toEqual([
      { project: "aaa", last: false, schemas: ["a"], children: [] },
      { project: "old", last: false, schemas: [], children: [{ project: "old-live", last: false, schemas: ["ol"], children: [] }] },
    ]);
  });

  it("filters to a project and its sub-projects, never No project", () => {
    const index = { projects: PROJECTS, bindings: BINDINGS };
    expect(outline(groupByProject(schemas, index, "finance"))).toEqual([
      {
        project: "finance",
        last: false,
        schemas: ["finance_shared"],
        children: [
          { project: "finance-dev", last: false, schemas: ["finance_dev"], children: [] },
          { project: "finance-prod", last: false, schemas: ["finance_prod"], children: [] },
          { project: "finance-old", last: true, schemas: ["finance_old"], children: [] },
        ],
      },
    ]);
    expect(outline(groupByProject(schemas, index, "finance-dev"))).toEqual([
      { project: "finance", last: false, schemas: [], children: [{ project: "finance-dev", last: false, schemas: ["finance_dev"], children: [] }] },
    ]);
    expect(groupByProject(schemas, index, "nope")).toEqual([]);
  });

  it("shows no headings when nothing is bound, or without a registry", () => {
    expect(groupByProject(schemas, { projects: PROJECTS, bindings: [] })).toEqual([{ kind: "none", schemas }]);
    expect(groupByProject(schemas, null)).toEqual([{ kind: "none", schemas }]);
    expect(groupByProject(schemas, null, "finance")).toEqual([{ kind: "none", schemas }]);
  });
});

describe("Tables tab with booth-core's project registry", () => {
  it("asks core directly with the user's token and workspace, and shows project headings", async () => {
    const calls = fakeCore({ ...tablesRoute(), ...registry() });
    mount();
    await screen.findByRole("link", { name: "players" });
    for (const path of ["/api/projects", "/api/projects/bindings"]) {
      const c = calls.find((x) => x.path === path)!;
      expect(c.headers.get("Authorization")).toBe("Bearer tok");
      expect(c.headers.get("X-Workspace")).toBe("acme");
    }
    expect(calls.find((x) => x.path === "/api/projects/bindings")!.search).toBe("?kind=postgres_schema");

    expect(headings()).toEqual([
      ["Finance4 tables", "true"],
      ["finance_shared1 table", "true"],
      ["Finance dev1 table", "true"],
      ["finance_dev1 table", "true"],
      ["Finance prod1 table", "true"],
      ["finance_prod1 table", "true"],
      ["Finance old1 table· archived", "false"],
      ["NFL1 table", "true"],
      ["nfl_data1 table", "true"],
      ["Legacy1 table· archived", "false"],
      ["No project2 tables", "true"],
      ["orphan1 table", "true"],
      ["public1 table", "true"],
    ]);
    // Archived projects start collapsed.
    expect(screen.queryByRole("link", { name: "archive" })).toBeNull();
    expect(screen.queryByRole("link", { name: "old_thing" })).toBeNull();
    const finance = screen.getByRole("region", { name: "Project Finance" });
    expect(within(finance).getAllByRole("link").map((a) => a.textContent)).toEqual(["accounts", "ledger", "ledger_prod"]);
    expect(within(screen.getByRole("region", { name: "No project" })).getAllByRole("link").map((a) => a.textContent)).toEqual(["lost", "recent"]);
    expect(screen.getByText("Finance", { selector: "span" })).toHaveAccessibleDescription(/doesn't change who can see/);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("opens an archived project on click, and a search finds tables in it", async () => {
    fakeCore({ ...tablesRoute(), ...registry() });
    mount();
    await screen.findByRole("link", { name: "players" });
    await userEvent.type(screen.getByRole("searchbox"), "old_th");
    expect(screen.getByRole("link", { name: "old_thing" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Project Finance" })).toBeNull();
    await userEvent.clear(screen.getByRole("searchbox"));
    expect(screen.queryByRole("link", { name: "old_thing" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: /^Legacy/ }));
    expect(screen.getByRole("link", { name: "old_thing" })).toBeInTheDocument();
  });

  it("with no bindings looks exactly as it does without a registry", async () => {
    fakeCore({ ...tablesRoute(), ...registry(PROJECTS, []) });
    mount();
    await screen.findByRole("link", { name: "players" });
    expect(screen.queryByRole("region", { name: /^Project / })).toBeNull();
    expect(screen.queryByRole("region", { name: "No project" })).toBeNull();
    expect(headings().map(([t]) => t)).toEqual(TABLES.map((r) => `${r.schema}1 table`));
  });

  const failures: [string, Record<string, () => Reply>][] = [
    ["404 (no registry)", {}],
    ["503 (registry not ready)", { "GET /api/projects": () => ({ status: 503, text: "projects unavailable" }), "GET /api/projects/bindings": () => ({ status: 503 }) }],
    ["a failed request", { "GET /api/projects": () => "throw", "GET /api/projects/bindings": () => "throw" }],
    ["only the bindings call failing", { "GET /api/projects": () => ({ json: PROJECTS }), "GET /api/projects/bindings": () => ({ status: 500 }) }],
    ["a 200 that isn't the registry (e.g. the shell's index.html)", { "GET /api/projects": () => ({ text: "<!doctype html>" }), "GET /api/projects/bindings": () => ({ text: "<!doctype html>" }) }],
    ["a 200 with the wrong shape", { "GET /api/projects": () => ({ json: { items: PROJECTS } }), "GET /api/projects/bindings": () => ({ json: BINDINGS }) }],
  ];
  it.each(failures)("falls back to plain schema groups, with no error, on %s", async (_, core) => {
    at("/database/tables?project=finance");
    fakeCore({ ...tablesRoute(), ...core });
    mount();
    await screen.findByRole("link", { name: "players" });
    expect(headings().map(([t]) => t)).toEqual(TABLES.map((r) => `${r.schema}1 table`));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(within(screen.getByRole("region", { name: "Tables" })).queryByText(/project/i)).toBeNull();
  });

  it("gives up on a core that doesn't answer", async () => {
    vi.useFakeTimers();
    vi.stubGlobal(
      "fetch",
      vi.fn((_: unknown, init?: RequestInit) => new Promise((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))))),
    );
    const index = loadProjectIndex({ workspace: "acme", getAccessToken: () => "tok" });
    vi.advanceTimersByTime(PROJECTS_TIMEOUT_MS);
    expect(await index).toBeNull();
  });
});

describe("?project= filter", () => {
  it("shows a selected parent with its sub-projects, and nothing unbound", async () => {
    at("/database/tables?project=finance");
    fakeCore({ ...tablesRoute(), ...registry() });
    mount();
    await screen.findByRole("link", { name: "accounts" });
    expect(screen.getByText(/Showing project/)).toHaveTextContent("Showing project Finance and its sub-projects. Projects only group this list; they don't change who can see what.");
    expect(headings().map(([t]) => t)).toEqual([
      "Finance4 tables",
      "finance_shared1 table",
      "Finance dev1 table",
      "finance_dev1 table",
      "Finance prod1 table",
      "finance_prod1 table",
      "Finance old1 table· archived",
    ]);
    expect(screen.queryByRole("link", { name: "players" })).toBeNull();
    expect(screen.queryByRole("region", { name: "No project" })).toBeNull();
    // In-module links keep the selection; the table's own path doesn't change.
    expect(screen.getByRole("link", { name: "accounts" })).toHaveAttribute("href", "/database/tables/finance_shared/accounts?project=finance");
  });

  it("shows only a selected sub-project's schemas, under its parent's heading", async () => {
    at("/database/tables?project=finance-dev");
    fakeCore({ ...tablesRoute(), ...registry() });
    mount();
    await screen.findByRole("link", { name: "ledger" });
    expect(headings().map(([t]) => t)).toEqual(["Finance1 table", "Finance dev1 table", "finance_dev1 table"]);
  });

  it("says so for a project that doesn't exist, and ignores a malformed slug", async () => {
    at("/database/tables?project=nope");
    fakeCore({ ...tablesRoute(), ...registry() });
    const { unmount } = mount();
    expect(await screen.findByText(/There's no project called nope/)).toBeInTheDocument();
    expect(screen.getByText("No tables in this project.")).toBeInTheDocument();
    unmount();

    at("/database/tables?project=Not%20A%20Slug");
    fakeCore({ ...tablesRoute(), ...registry() });
    mount();
    await screen.findByRole("link", { name: "players" });
    expect(screen.getByRole("region", { name: "No project" })).toBeInTheDocument();
  });

  it("follows the shell changing the selection", async () => {
    fakeCore({ ...tablesRoute(), ...registry() });
    mount();
    await screen.findByRole("link", { name: "players" });
    window.history.pushState({}, "", "/database/tables?project=nfl");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(await screen.findByText(/Showing project/)).toHaveTextContent("Showing project NFL");
    expect(screen.queryByRole("link", { name: "accounts" })).toBeNull();
    expect(screen.getByRole("link", { name: "players" })).toBeInTheDocument();
  });
});
