// Public entry point for @projectbooth/database-ui (ADR 0030, ADR 0093). Consumers also import the
// stylesheet once: `@projectbooth/database-ui/dist/style.css` (see README).
import "./library.css";

export { DatabaseApp } from "./DatabaseApp";
export type { DatabaseAppProps } from "./DatabaseApp";
export type { WorkspaceRole } from "./types";
