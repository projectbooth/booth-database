import type { ListedDatabase, QueryResult, StatusResponse, TablesResponse } from "../types";

// Mounted inside booth-design's shell, so requests resolve against the shell's origin and must go
// through booth-core's gateway at /modules/{id}/*, which strips the prefix before forwarding to
// this module's own /api/* routes. A bare "/api/..." would hit booth-core's own API instead.
const BASE = "/modules/database/api";

export type GetAccessToken = () => string | null;

/** Everything a request needs from the mounting shell (ADR 0031/0033). */
export interface ApiContext {
  workspace: string;
  getAccessToken: GetAccessToken;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    /** The server's machine-readable code for a query failure (internal/query), if any. */
    public code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// X-Workspace is required by the gateway on every authenticated request (ADR 0025).
// getAccessToken is called fresh before each request, never cached (ADR 0033); a null token omits
// the header rather than sending "Bearer null".
async function request<T>(ctx: ApiContext, method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const headers = new Headers({ "X-Workspace": ctx.workspace });
  const token = ctx.getAccessToken();
  if (token !== null) headers.set("Authorization", `Bearer ${token}`);
  if (body !== undefined) headers.set("Content-Type", "application/json");
  const res = await fetch(BASE + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal });
  if (!res.ok) {
    const text = await res.text();
    let message = text || `HTTP ${res.status}`;
    let code: string | undefined;
    try {
      const parsed = JSON.parse(text) as { error?: string; code?: string };
      if (parsed.error) message = parsed.error;
      code = parsed.code;
    } catch {
      // not JSON — e.g. a gateway error page
    }
    throw new ApiError(res.status, message, code);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

const get = <T>(ctx: ApiContext, path: string) => request<T>(ctx, "GET", path);

export const getStatus = (ctx: ApiContext) => get<StatusResponse>(ctx, "/status");

export const listDatabases = (ctx: ApiContext) => get<{ items: ListedDatabase[] }>(ctx, "/databases").then((r) => r.items);

/** ADR 0116: the workspace database's tables, views and materialized views. Any member. */
export const listTables = (ctx: ApiContext) => get<TablesResponse>(ctx, "/tables");

/** The first rows (at most 100) of one table, run read-only as a viewer whatever the caller's role. */
export const previewTable = (ctx: ApiContext, schema: string, name: string) =>
  get<QueryResult>(ctx, `/tables/preview?${new URLSearchParams({ schema, name }).toString()}`);

/** Runs one read-only statement in the SQL console. */
export const runQuery = (ctx: ApiContext, sql: string) => request<QueryResult>(ctx, "POST", "/query", { sql });

/** Cancels the caller's running console query, if any. Resolves once the server has released it. */
export const cancelQuery = (ctx: ApiContext) => request<void>(ctx, "DELETE", "/query");
