import type { ListedDatabase, StatusResponse } from "../types";

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
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// X-Workspace is required by the gateway on every authenticated request (ADR 0025).
// getAccessToken is called fresh before each request, never cached (ADR 0033); a null token omits
// the header rather than sending "Bearer null".
async function get<T>(ctx: ApiContext, path: string): Promise<T> {
  const headers = new Headers({ "X-Workspace": ctx.workspace });
  const token = ctx.getAccessToken();
  if (token !== null) headers.set("Authorization", `Bearer ${token}`);
  const res = await fetch(BASE + path, { headers });
  if (!res.ok) {
    const text = await res.text();
    let message = text || `HTTP ${res.status}`;
    try {
      const body = JSON.parse(text) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // not JSON — e.g. a gateway error page
    }
    throw new ApiError(res.status, message);
  }
  return (await res.json()) as T;
}

export const getStatus = (ctx: ApiContext) => get<StatusResponse>(ctx, "/status");

export const listDatabases = (ctx: ApiContext) => get<{ items: ListedDatabase[] }>(ctx, "/databases").then((r) => r.items);
