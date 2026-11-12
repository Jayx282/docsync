import type {
  CompiledFragment,
  Connection,
  Design,
  DeployPlan,
  FilterCount,
  PreviewResult,
  Profile,
  Relation,
  RuntimeStatus,
  Suggestion,
  TableDetail,
  TableRow,
  ValidateResult,
} from "./types";

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

const events = new EventTarget();

/** Calls fn when the studio rejects this browser's token (for example after a restart). */
export function onUnauthorized(fn: () => void): () => void {
  events.addEventListener("unauthorized", fn);
  return () => events.removeEventListener("unauthorized", fn);
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: "same-origin" };
  if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }
  let res: Response;
  try {
    res = await fetch(path, init);
  } catch {
    throw new ApiError("Cannot reach docsync studio. Is it still running?", 0);
  }
  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (res.status === 401) events.dispatchEvent(new Event("unauthorized"));
  if (!res.ok) {
    const msg =
      data && typeof data === "object" && "error" in data
        ? String((data as { error: unknown }).error)
        : `${res.status} ${res.statusText}`;
    throw new ApiError(msg, res.status);
  }
  return data as T;
}

const enc = encodeURIComponent;

export const api = {
  connection: () => call<Connection>("GET", "/api/studio/connection"),
  refreshSchema: () => call<{ tables: number; loaded_at: string }>("POST", "/api/studio/schema/refresh", {}),
  tables: () => call<TableRow[]>("GET", "/api/studio/tables"),
  table: (name: string) => call<TableDetail>("GET", `/api/studio/tables/${enc(name)}`),
  suggestions: (root: string) => call<Suggestion[]>("GET", `/api/studio/suggestions?root=${enc(root)}`),
  profile: (root: string, relation: Relation) => call<Profile>("POST", "/api/studio/profile", { root, relation }),

  designs: () => call<Design[]>("GET", "/api/studio/designs"),
  design: (name: string) => call<Design>("GET", `/api/studio/designs/${enc(name)}`),
  saveDesign: (d: Design) => call<Design>("PUT", `/api/studio/designs/${enc(d.name)}`, d),
  deleteDesign: (name: string) => call<{ result: string }>("DELETE", `/api/studio/designs/${enc(name)}`),

  compile: (design: Design) =>
    call<{ errors: string[]; yaml: string; sql: CompiledFragment[] }>("POST", "/api/studio/compile", { design }),
  preview: (design: Design, ids: string[], limit = 3) =>
    call<PreviewResult>("POST", "/api/studio/preview", { design, ids, limit }),
  validate: (design: Design) => call<ValidateResult>("POST", "/api/studio/validate", { design }),
  deploy: (design: Design) => call<RuntimeStatus>("POST", "/api/studio/deploy", { design }),
  deployPlan: (design: Design) => call<DeployPlan>("POST", "/api/studio/deploy/plan", { design }),
  filterCount: (design: Design) => call<FilterCount>("POST", "/api/studio/filter/count", { design }),

  runtime: () => call<RuntimeStatus>("GET", "/api/studio/runtime"),
  stopRuntime: () => call<RuntimeStatus>("POST", "/api/studio/runtime/stop", {}),
};

export function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
