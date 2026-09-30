import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DatabaseApp } from "../DatabaseApp";
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

  it("never calls the API for a non-owner", async () => {
    for (const role of ["editor", "viewer"] as const) {
      const calls = mockFetch({});
      const { unmount } = mount(role);
      expect(screen.getByText(/visible to workspace owners/)).toBeInTheDocument();
      // The connect snippet is still useful to everyone.
      expect(screen.getByText(/booth_database.connect\(\)/)).toBeInTheDocument();
      await new Promise((r) => setTimeout(r, 0));
      expect(calls).toHaveLength(0);
      unmount();
    }
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
