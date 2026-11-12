import { api } from "../../api";
import { loadTable } from "../../hooks";
import type { Design, Field, Relation, Suggestion, Table, TableRow } from "../../types";
import { eqi, fmtCompact, lc, outputName, plural, shortName, toId, uniqueName } from "../../util";
import type { Option } from "../../components/ui";

/** Comparable form of a design (ignores the save time). */
export function designKey(d: Design): string {
  return JSON.stringify({ ...d, updated_at: undefined });
}

/** Fills in fields the Go side may send as null, so edits never hit undefined. */
export function normalizeDesign(d: Design): Design {
  return {
    version: d.version || 1,
    name: d.name,
    root: { table: d.root?.table ?? "", key: d.root?.key ?? "", fields: d.root?.fields ?? [], filter: d.root?.filter ?? "" },
    related: (d.related ?? []).map((r) => ({
      id: r.id,
      table: r.table,
      shape: r.shape,
      join: r.join ?? [],
      ...(r.via ? { via: { table: r.via.table, join: r.via.join ?? [] } } : {}),
      fields: r.fields ?? [],
      path: r.path ?? "",
      sort_by: r.sort_by ?? [],
      ...(r.hidden ? { hidden: true } : {}),
    })),
    calculated: (d.calculated ?? []).map((c) => ({ name: c.name ?? "", expr: c.expr ?? "" })),
    output: { format: d.output?.format || "parquet_log", include_json: d.output?.include_json ?? false },
  };
}

/** Lower-cased names already used at the top level of documents. */
export function topLevelNames(d: Design, except?: number): Set<string> {
  const s = new Set<string>();
  d.root.fields.forEach((f) => s.add(lc(outputName(f))));
  d.related.forEach((r, i) => {
    if (i === except || r.hidden) return;
    if (r.shape === "list" || r.path) {
      if (r.path) s.add(lc(r.path));
    } else {
      r.fields.forEach((f) => s.add(lc(outputName(f))));
    }
  });
  (d.calculated ?? []).forEach((c) => c.name && s.add(lc(c.name)));
  return s;
}

/** Errors from the server that belong to one calculated field, by field name. */
export function calcErrors(errors: string[]): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const e of errors) {
    const m = /^calculated field "([^"]*)": (.*)$/.exec(e);
    if (!m) continue;
    const list = out.get(m[1]) ?? [];
    list.push(m[2]);
    out.set(m[1], list);
  }
  return out;
}

/** Errors from the server about the root filter. */
export function filterErrors(errors: string[]): string[] {
  return errors.filter((e) => e.startsWith("filter: ")).map((e) => e.slice("filter: ".length));
}

/** Lower-cased tables used by the design (a table can appear only once for now). */
export function usedTables(d: Design, except?: number): Set<string> {
  const s = new Set<string>([lc(d.root.table)]);
  d.related.forEach((r, i) => {
    if (i === except) return;
    s.add(lc(r.table));
    if (r.via) s.add(lc(r.via.table));
  });
  return s;
}

function relationIds(d: Design): Set<string> {
  const s = new Set(["root"]);
  d.related.forEach((r) => s.add(r.id));
  return s;
}

export function defaultRootFields(t: Table): Field[] {
  const cols = t.columns.length <= 20 ? t.columns : t.columns.filter((c) => t.primary_key.some((k) => eqi(k, c.name)));
  return cols.map((c) => ({ column: c.name }));
}

export function newDesign(name: string, t: Table): Design {
  return {
    version: 1,
    name,
    root: { table: t.name, key: t.primary_key[0], fields: defaultRootFields(t) },
    related: [],
    output: { format: "parquet_log", include_json: false },
  };
}

export function changeRoot(d: Design, t: Table): Design {
  return { ...d, root: { table: t.name, key: t.primary_key[0], fields: defaultRootFields(t) }, related: [] };
}

/**
 * Columns preselected for a new relation: up to eight, leaving out the columns that
 * only repeat the join. Top-level fields from lookups get the relation's name as a
 * prefix ("lab_name"), so they read clearly and do not collide with root fields.
 */
export function defaultFields(d: Design, r: Relation, t: Table): Field[] {
  const skip = new Set(r.via ? [] : r.join.map((j) => lc(j.to)));
  const cols = t.columns.filter((c) => !skip.has(lc(c.name))).slice(0, 8);
  if (r.shape === "list" || r.path) return cols.map((c) => ({ column: c.name }));
  const taken = topLevelNames(d);
  return cols.map((c) => {
    let name = lc(c.name).startsWith(r.id + "_") ? c.name : `${r.id}_${c.name}`;
    name = uniqueName(name, taken);
    taken.add(lc(name));
    return name === c.name ? { column: c.name } : { column: c.name, as: name };
  });
}

export function newRelation(d: Design, base: Omit<Relation, "id" | "fields" | "path" | "sort_by">, t: Table): Relation {
  const id = uniqueName(toId(shortName(base.table)), relationIds(d));
  const r: Relation = { ...base, id, fields: [], path: "", sort_by: [] };
  if (r.shape === "list") r.path = uniqueName(plural(id), topLevelNames(d));
  r.fields = defaultFields(d, r, t);
  return r;
}

export function relationFromSuggestion(d: Design, s: Suggestion, t: Table): Relation {
  return newRelation(d, { table: s.table, shape: s.shape, join: s.join, ...(s.via ? { via: s.via } : {}) }, t);
}

/** Options for choosing a root table: only single-column primary keys qualify. */
export function rootOptions(tables: TableRow[] | undefined): Option[] {
  return (tables ?? []).map((t) => ({
    value: t.name,
    hint: `${fmtCompact(t.rows)} rows · key ${t.primary_key.join(", ") || "none"}`,
    disabled:
      t.primary_key.length === 1
        ? undefined
        : t.primary_key.length === 0
          ? "no primary key"
          : `composite key (${t.primary_key.join(", ")})`,
  }));
}

/** Creates and saves a design rooted at table, returning its name. */
export async function createDesignFor(table: string): Promise<string> {
  const [detail, designs] = await Promise.all([loadTable(table), api.designs()]);
  const name = uniqueName(toId(shortName(detail.table.name)), new Set(designs.map((d) => d.name)));
  await api.saveDesign(newDesign(name, detail.table));
  return name;
}

// ---- unsaved drafts survive switching tabs and designs (not page reloads) -------------

interface Draft {
  design: Design;
  saved: string; // designKey of the last saved version
}

const draftMap = new Map<string, Draft>();
const draftListeners = new Set<() => void>();
let draftVersion = 0;

export const drafts = {
  get: (name: string) => draftMap.get(name),
  has: (name: string) => draftMap.has(name),
  set(name: string, d: Draft | null) {
    const had = draftMap.has(name);
    if (d) draftMap.set(name, d);
    else draftMap.delete(name);
    if (had !== !!d) {
      draftVersion++;
      draftListeners.forEach((l) => l());
    }
  },
  subscribe(cb: () => void) {
    draftListeners.add(cb);
    return () => {
      draftListeners.delete(cb);
    };
  },
  version: () => draftVersion,
};
