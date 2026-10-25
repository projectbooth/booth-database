import { useSyncExternalStore } from "react";

// The shell (booth-design) matches a module's navPath — and anything beneath it (a longest-prefix
// match) — to the one component registered for the module id, but NativeModuleProps (ADR 0031/
// 0033) carries no route. So this package reads window.location itself, the same workaround
// booth-catalog and booth-storage use. If the shell ever passes a route prop, this file is the
// only place that changes.
//
// The deep links below are a contract: booth-catalog links to them from a postgres dataset
// (ADR 0116 fork A). Keep them stable.
//
//   /database                          overview (status, connect snippets)
//   /database/tables                   the Tables tab
//   /database/tables/<schema>/<name>   one table: columns and a 100-row preview
//   /database/sql                      the SQL console
//   /database/sql?schema=<s>&name=<n>  the console, prefilled with a SELECT from that table
//
// <schema> and <name> are each encodeURIComponent-encoded, so any PostgreSQL name round-trips.
//
// ADR 0124: the shell's project picker adds ?project=<slug> to the page URL. It is a list filter,
// never part of a path: the Tables tab reads it, and in-module links carry it along (hrefFor) so
// moving between tabs and tables keeps the selection.

export const BASE_PATH = "/database";

export type Route =
  | { name: "overview" }
  | { name: "tables" }
  | { name: "table"; schema: string; table: string }
  | { name: "sql"; schema?: string; table?: string };

export type Tab = "overview" | "tables" | "sql";

export function tabOf(route: Route): Tab {
  return route.name === "table" ? "tables" : route.name;
}

function decode(s: string): string | null {
  try {
    return decodeURIComponent(s);
  } catch {
    return null;
  }
}

/** Parses the address bar. Anything unrecognised is the overview: a stale bookmark should land
 *  somewhere useful, not on an error. */
export function parseRoute(pathname: string, search: string): Route {
  const rel = pathname === BASE_PATH ? "" : pathname.startsWith(BASE_PATH + "/") ? pathname.slice(BASE_PATH.length + 1) : "";
  // Split before decoding, so an encoded "/" inside a name stays inside it.
  const parts = rel.split("/").filter(Boolean);
  const [area, schema, table] = parts;
  if (area === "tables") {
    if (parts.length === 1) return { name: "tables" };
    if (parts.length === 3) {
      const s = decode(schema);
      const t = decode(table);
      if (s && t) return { name: "table", schema: s, table: t };
    }
    return { name: "tables" };
  }
  if (area === "sql" && parts.length === 1) {
    const q = new URLSearchParams(search);
    const s = q.get("schema");
    const t = q.get("name");
    return s && t ? { name: "sql", schema: s, table: t } : { name: "sql" };
  }
  return { name: "overview" };
}

const e = encodeURIComponent;

/** The inverse of parseRoute. */
export function routePath(route: Route): string {
  switch (route.name) {
    case "overview":
      return BASE_PATH;
    case "tables":
      return `${BASE_PATH}/tables`;
    case "table":
      return `${BASE_PATH}/tables/${e(route.schema)}/${e(route.table)}`;
    case "sql":
      return route.schema && route.table ? `${BASE_PATH}/sql?${new URLSearchParams({ schema: route.schema, name: route.table }).toString()}` : `${BASE_PATH}/sql`;
  }
}

/** The project slug grammar (contracts/projects.md). Anything else is ignored, as if absent. */
const PROJECT_SLUG = /^[a-z0-9-]{1,63}$/;

/** The ?project= filter in a query string, or null. */
export function projectParam(search: string): string | null {
  const v = new URLSearchParams(search).get("project");
  return v !== null && PROJECT_SLUG.test(v) ? v : null;
}

/** A route's href, keeping the current ?project= selection. */
export function hrefFor(route: Route): string {
  const project = projectParam(window.location.search);
  if (project === null) return routePath(route);
  const path = routePath(route);
  return `${path}${path.includes("?") ? "&" : "?"}${new URLSearchParams({ project }).toString()}`;
}

const NAVIGATE_EVENT = "booth-database:navigate";

function subscribe(onChange: () => void): () => void {
  window.addEventListener("popstate", onChange);
  window.addEventListener(NAVIGATE_EVENT, onChange);
  return () => {
    window.removeEventListener("popstate", onChange);
    window.removeEventListener(NAVIGATE_EVENT, onChange);
  };
}

/** The current route, re-rendering on back/forward and on this package's own navigations. */
export function useRoute(): Route {
  const pathname = useSyncExternalStore(subscribe, () => window.location.pathname, () => "/");
  const search = useSyncExternalStore(subscribe, () => window.location.search, () => "");
  return parseRoute(pathname, search);
}

/** The shell's current ?project= selection (ADR 0124), or null for all projects. */
export function useProjectParam(): string | null {
  const search = useSyncExternalStore(subscribe, () => window.location.search, () => "");
  return projectParam(search);
}

/** Navigates inside the shell without a page reload (a reload would drop booth-design's
 *  in-memory token, ADR 0032): pushState, then a synthetic popstate the shell's router hears. */
export function navigate(route: Route): void {
  window.history.pushState({}, "", hrefFor(route));
  window.dispatchEvent(new PopStateEvent("popstate"));
  window.dispatchEvent(new Event(NAVIGATE_EVENT));
}

/** booth-api's page in the shell (its manifest navPath; not /api, which is booth-core's). */
export const API_MODULE_PATH = "/apis";

/** Navigates to another module's page inside the shell, without a reload (see navigate). */
export function navigateTo(path: string): void {
  window.history.pushState({}, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
}
