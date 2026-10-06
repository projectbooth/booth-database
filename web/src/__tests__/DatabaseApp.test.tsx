import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DatabaseApp, NOTEBOOK_SNIPPET, PIPELINE_SNIPPET } from "../DatabaseApp";
import { formatBytes, formatCreated } from "../format";
import type { WorkspaceRole } from "../types";

interface Call {
  path: string;
  headers: Headers;
}

/** Replaces fetch with a router keyed by path; anything unrouted is a recorded 404. */
function mockFetch(routes: Record<string, { status?: number; json?: unknown; text?: string } | (() => { status?: number; json?: unknown })>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input), "http://localhost").pathname;
      calls.push({ path, headers: new Headers(init?.headers) });
      const r = routes[path];
      const reply = r === undefined ? { status: 404, text: "not routed" } : typeof r === "function" ? r() : r;
      const body = "json" in reply && reply.json !== undefined ? JSON.stringify(reply.json) : ((reply as { text?: string }).text ?? "");
      return new Response(body, { status: reply.status ?? 200 });
    }),
  );
  return calls;
}

function mount(role: WorkspaceRole = "owner", token: string | null = "tok") {
  return render(<DatabaseApp workspace="acme" role={role} theme="light" getAccessToken={() => token} />);
}

const DB = {
  database: "bdb_ws_0123456789abcdef01234567",
  createdAt: "2026-09-30T12:00:00Z",
  sizeBytes: 9 * 1024 * 1024,
  activeCredentials: 2,
  openConnections: 1,
  tables: 4,
};

afterEach(() => vi.unstubAllGlobals());

describe("DatabaseApp", () => {
  it("shows an owner their workspace's database status", async () => {
    const calls = mockFetch({ "/modules/database/api/status": { json: { workspace: "acme", provisioned: true, database: DB, operator: false } } });
    mount();
    expect(await screen.findByText("9.0 MB")).toBeInTheDocument();
    expect(screen.getByText("4")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
    expect(screen.getByText(DB.database)).toBeInTheDocument();
    // Through the gateway, with the workspace header and a fresh token (ADR 0025/0033).
    expect(calls).toHaveLength(1);
    expect(calls[0].headers.get("X-Workspace")).toBe("acme");
    expect(calls[0].headers.get("Authorization")).toBe("Bearer tok");
    // Not an operator: the cross-workspace listing is neither shown nor fetched.
    expect(screen.queryByText("All workspace databases")).not.toBeInTheDocument();
  });

  it("explains a workspace with no database yet", async () => {
    mockFetch({ "/modules/database/api/status": { json: { workspace: "acme", provisioned: false, database: null, operator: false } } });
    mount();
    expect(await screen.findByText("No database yet")).toBeInTheDocument();
  });

  it("only probes the operator listing for a non-owner, and a 403 shows nothing more", async () => {
    for (const role of ["editor", "viewer"] as const) {
      const calls = mockFetch({ "/modules/database/api/databases": { status: 403, json: { error: "limited to platform operators" } } });
      const { unmount } = mount(role);
      expect(screen.getByText(/visible to its owners/)).toBeInTheDocument();
      // The connect snippet is still useful to everyone.
      expect(screen.getByTestId("snippet-notebook")).toBeInTheDocument();
      await waitFor(() => expect(calls).toHaveLength(1));
      await new Promise((r) => setTimeout(r, 0));
      // Never the owner-only status route, and no error or listing for the normal 403.
      expect(calls.map((c) => c.path)).toEqual(["/modules/database/api/databases"]);
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
      expect(screen.queryByText("All workspace databases")).not.toBeInTheDocument();
      unmount();
    }
  });

  it("shows a non-owner platform operator every database (ADR 0094)", async () => {
    mockFetch({ "/modules/database/api/databases": { json: { items: [{ ...DB, workspace: "acme", tables: undefined }] } } });
    mount("viewer");
    expect(await screen.findByText("All workspace databases")).toBeInTheDocument();
    expect(screen.getByText("1 database")).toBeInTheDocument();
    expect(screen.getByText(/visible to its owners/)).toBeInTheDocument();
  });

  it("omits Authorization when the shell has no token", async () => {
    const calls = mockFetch({ "/modules/database/api/status": { json: { workspace: "acme", provisioned: false, database: null, operator: false } } });
    mount("owner", null);
    await screen.findByText("No database yet");
    expect(calls[0].headers.has("Authorization")).toBe(false);
  });

  it("shows operators every database, naming only their own workspaces", async () => {
    mockFetch({
      "/modules/database/api/status": { json: { workspace: "platform", provisioned: false, database: null, operator: true } },
      "/modules/database/api/databases": {
        json: {
          items: [
            { ...DB, workspace: "acme", tables: undefined },
            { ...DB, database: "bdb_ws_ffffffffffffffffffffffff", sizeBytes: 1024, createdAt: null, tables: undefined },
          ],
        },
      },
    });
    mount();
    expect(await screen.findByText("All workspace databases")).toBeInTheDocument();
    expect(await screen.findByText("acme")).toBeInTheDocument();
    expect(screen.getByText("bdb_ws_ffffffffffffffffffffffff")).toBeInTheDocument();
    expect(screen.getByText("2 databases")).toBeInTheDocument();
    expect(screen.getByText("Unknown")).toBeInTheDocument();
  });

  it("offers no destructive action anywhere (ADR 0093)", async () => {
    mockFetch({
      "/modules/database/api/status": { json: { workspace: "platform", provisioned: true, database: DB, operator: true } },
      "/modules/database/api/databases": { json: { items: [DB] } },
    });
    mount();
    await screen.findByText("All workspace databases");
    await screen.findByText("1 database");
    const buttons = screen.getAllByRole("button").map((b) => b.textContent);
    expect(buttons).toEqual(["Refresh"]);
  });

  it("explains a 503 (no identity provider) and a 403", async () => {
    mockFetch({ "/modules/database/api/status": { status: 503, json: { error: "unavailable" } } });
    const { unmount } = mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/no identity provider/);
    unmount();
    mockFetch({ "/modules/database/api/status": { status: 403, json: { error: "only a workspace owner can view its database status" } } });
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/don't have access/);
  });

  it("refreshes on demand without a new token being cached", async () => {
    let n = 0;
    const calls = mockFetch({
      "/modules/database/api/status": () => ({ json: { workspace: "acme", provisioned: true, database: { ...DB, activeCredentials: ++n }, operator: false } }),
    });
    mount();
    await screen.findByText("9.0 MB");
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(calls).toHaveLength(2));
  });

  it("re-fetches for a new workspace", async () => {
    const calls = mockFetch({ "/modules/database/api/status": { json: { workspace: "acme", provisioned: false, database: null, operator: false } } });
    const { rerender } = render(<DatabaseApp workspace="acme" role="owner" theme="dark" getAccessToken={() => "t"} />);
    await screen.findByText("No database yet");
    rerender(<DatabaseApp workspace="globex" role="owner" theme="dark" getAccessToken={() => "t"} />);
    await waitFor(() => expect(calls.map((c) => c.headers.get("X-Workspace"))).toEqual(["acme", "globex"]));
  });
});

describe("connect snippet", () => {
  // Since ADR 0095 the notebook and pipeline-task images don't ship the booth_database client;
  // the in-cluster path is the credential sidecar's DATABASE_URL.
  it("never tells anyone to import booth_database", () => {
    mockFetch({});
    mount("viewer");
    const section = screen.getByRole("heading", { name: "Connect from a notebook or pipeline task" }).closest("section")!;
    expect(section.textContent).not.toContain("booth_database");
    expect(section.textContent).not.toContain("read_only");
    expect(NOTEBOOK_SNIPPET).not.toContain("booth_database");
    expect(PIPELINE_SNIPPET).not.toContain("booth_database");
  });

  it("shows the notebook and pipeline paths that work today", () => {
    mockFetch({});
    mount("viewer");
    expect(screen.getByTestId("snippet-notebook").textContent).toBe(
      ["import booth.database, pandas as pd", "", "engine = booth.database.engine()", 'pd.read_sql("SELECT now()", engine)'].join("\n"),
    );
    const pipeline = screen.getByTestId("snippet-pipeline").textContent!;
    expect(pipeline).toContain("import os, psycopg");
    expect(pipeline).toContain('psycopg.connect(os.environ["DATABASE_URL"])');
    expect(screen.getByText(/valid for about an hour/)).toBeInTheDocument();
    expect(screen.getByText(/ends when the credential it opened with expires/)).toBeInTheDocument();
  });
});

describe("format", () => {
  it("formats sizes and times", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(3 * 1024 ** 3)).toBe("3.0 GB");
    expect(formatCreated(null)).toBe("Unknown");
    expect(formatCreated("nonsense")).toBe("Unknown");
    expect(formatCreated("2026-09-30T12:00:00Z")).toMatch(/2026/);
  });
});
