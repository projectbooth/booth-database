import { useCallback, useEffect, useRef, useState } from "react";
import { listProjects, listSchemaBindings, type ApiContext } from "./api/client";
import type { ProjectIndex } from "./grouping";

export type LoadState<T> = { status: "loading" } | { status: "error"; error: string; httpStatus?: number } | { status: "ready"; data: T };

/** A hand-rolled fetch state machine (loading → ready | error), as ARCHITECTURE.md §6 recommends
 *  over a data-fetching library — the same shape booth-catalog uses. Reruns when `deps` change and
 *  ignores a response that arrives after a newer request started, so a quick workspace switch can
 *  never show the previous workspace's figures. `enabled: false` skips loading entirely. */
export function useLoad<T>(load: () => Promise<T>, deps: unknown[], enabled = true): { state: LoadState<T>; reload: () => void } {
  const [state, setState] = useState<LoadState<T>>({ status: "loading" });
  const latest = useRef(load);
  latest.current = load;
  const generation = useRef(0);

  const run = useCallback(
    (showSpinner: boolean) => {
      if (!enabled) return;
      const mine = ++generation.current;
      if (showSpinner) setState({ status: "loading" });
      latest.current().then(
        (data) => {
          if (mine === generation.current) setState({ status: "ready", data });
        },
        (err: unknown) => {
          if (mine !== generation.current) return;
          const httpStatus = typeof err === "object" && err !== null && "status" in err ? Number((err as { status: unknown }).status) : undefined;
          setState({ status: "error", error: err instanceof Error ? err.message : String(err), httpStatus });
        },
      );
    },
    [enabled],
  );

  useEffect(() => {
    run(true);
  }, [...deps, run]);

  const reload = useCallback(() => run(false), [run]);
  return { state, reload };
}

/** How long the Tables tab waits for booth-core's project registry before showing the list
 *  without project headings (contracts/projects.md: never block the page). */
export const PROJECTS_TIMEOUT_MS = 5000;

/** Loads booth-core's projects and postgres_schema bindings. Never rejects: 404 (no registry),
 *  503 (registry not ready), a timeout, an unexpected shape or any other failure is null, and the
 *  Tables tab then looks exactly as it does without a registry. */
export async function loadProjectIndex(api: ApiContext): Promise<ProjectIndex | null> {
  const abort = new AbortController();
  const timer = setTimeout(() => abort.abort(), PROJECTS_TIMEOUT_MS);
  try {
    const [projects, bindings] = await Promise.all([listProjects(api, abort.signal), listSchemaBindings(api, abort.signal)]);
    if (!Array.isArray(projects) || !Array.isArray(bindings)) return null;
    return { projects, bindings };
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
}
