import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import { api, message } from "./api";
import type { TableDetail, TableRow } from "./types";

// ---- hash routing: #/explore/dbo.stone, #/design/stone, #/pipeline ------------------

function subscribeHash(cb: () => void) {
  window.addEventListener("hashchange", cb);
  return () => window.removeEventListener("hashchange", cb);
}
const getHash = () => window.location.hash;

export function useRoute(): string[] {
  const hash = useSyncExternalStore(subscribeHash, getHash);
  return hash
    .replace(/^#\/?/, "")
    .split("/")
    .filter(Boolean)
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    });
}

export function go(path: string) {
  window.location.hash = "#/" + path.split("/").map(encodeURIComponent).join("/");
}

export function href(path: string): string {
  return "#/" + path.split("/").map(encodeURIComponent).join("/");
}

// ---- schema cache, invalidated by "Refresh schema" ----------------------------------

let schemaVersion = 0;
const schemaListeners = new Set<() => void>();
let tablesPromise: Promise<TableRow[]> | null = null;
const tableCache = new Map<string, Promise<TableDetail>>();

function subscribeSchema(cb: () => void) {
  schemaListeners.add(cb);
  return () => {
    schemaListeners.delete(cb);
  };
}
const getSchemaVersion = () => schemaVersion;

export function schemaChanged() {
  schemaVersion++;
  tablesPromise = null;
  tableCache.clear();
  schemaListeners.forEach((l) => l());
}

export function useSchemaVersion(): number {
  return useSyncExternalStore(subscribeSchema, getSchemaVersion);
}

export function loadTables(): Promise<TableRow[]> {
  if (!tablesPromise) {
    const p = api.tables();
    tablesPromise = p;
    p.catch(() => {
      if (tablesPromise === p) tablesPromise = null;
    });
  }
  return tablesPromise;
}

export function loadTable(name: string): Promise<TableDetail> {
  const k = name.toLowerCase();
  let p = tableCache.get(k);
  if (!p) {
    const np = api.table(name);
    p = np;
    tableCache.set(k, np);
    np.catch(() => {
      if (tableCache.get(k) === np) tableCache.delete(k);
    });
  }
  return p;
}

// ---- async state ---------------------------------------------------------------------

export interface Async<T> {
  data?: T;
  error?: string;
  loading: boolean;
  reload: () => void;
}

/**
 * Runs fn whenever deps change. With keep, the previous data stays visible while
 * reloading (used for lists that refresh in place).
 */
export function useAsync<T>(fn: (() => Promise<T>) | null, deps: unknown[], keep = false): Async<T> {
  const [state, setState] = useState<{ data?: T; error?: string; loading: boolean }>({ loading: fn != null });
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!fn) {
      setState({ loading: false });
      return;
    }
    let live = true;
    setState((s) => (keep ? { data: s.data, loading: true } : { loading: true }));
    fn().then(
      (data) => live && setState({ data, loading: false }),
      (e: unknown) => live && setState((s) => ({ data: keep ? s.data : undefined, error: message(e), loading: false })),
    );
    return () => {
      live = false;
    };
    // fn is recreated on every render; deps say when to rerun it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);
  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { ...state, reload };
}

export function useTables(): Async<TableRow[]> {
  const v = useSchemaVersion();
  return useAsync(() => loadTables(), [v]);
}

export function useTable(name: string | undefined): Async<TableDetail> {
  const v = useSchemaVersion();
  return useAsync(name ? () => loadTable(name) : null, [name?.toLowerCase(), v]);
}
