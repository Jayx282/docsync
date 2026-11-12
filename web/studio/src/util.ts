import type { Field, Table } from "./types";

export const lc = (s: string) => s.toLowerCase();
export const eqi = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();

/** "dbo.stone" -> "stone" */
export function shortName(t: string): string {
  const i = t.lastIndexOf(".");
  return i >= 0 ? t.slice(i + 1) : t;
}

/** "dbo.stone" -> "dbo" */
export function schemaOf(t: string): string {
  const i = t.lastIndexOf(".");
  return i >= 0 ? t.slice(0, i) : "";
}

export function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

export function fmtCompact(n: number): string {
  const f = (v: number, d: number) => v.toFixed(d).replace(/\.0$/, "");
  if (n < 1000) return String(n);
  if (n < 1e6) return f(n / 1e3, n < 1e4 ? 1 : 0) + "k";
  if (n < 1e9) return f(n / 1e6, n < 1e7 ? 1 : 0) + "M";
  return f(n / 1e9, 1) + "B";
}

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

export function fmtTime(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return "";
  return d.toLocaleString();
}

export function outputName(f: Field): string {
  return f.as || f.column;
}

/** Same rule as the Go suggester. */
export function plural(s: string): string {
  if (s.endsWith("s") || s.endsWith("x")) return s + "es";
  if (s.endsWith("y") && s.length > 1 && !"aeiou".includes(s[s.length - 2])) return s.slice(0, -1) + "ies";
  return s + "s";
}

/** Turns a table name into a design or relation id (^[a-z][a-z0-9_]*$). */
export function toId(base: string): string {
  let id = base
    .toLowerCase()
    .replace(/[^a-z0-9_]+/g, "_")
    .replace(/^_+|_+$/g, "");
  if (!id || id[0] < "a" || id[0] > "z") id = "t_" + id;
  return id.slice(0, 50);
}

/** base, base_2, base_3, ... whichever is not in used (compared lowercase). */
export function uniqueName(base: string, used: Set<string>): string {
  let cand = base;
  for (let i = 2; used.has(cand.toLowerCase()); i++) cand = `${base}_${i}`;
  return cand;
}

/** Do cols identify at most one row? (They cover the primary key or match a unique index.) */
export function isUnique(t: Table, cols: string[]): boolean {
  if (cols.length === 0) return false;
  const has = (list: string[], c: string) => list.some((x) => eqi(x, c));
  if (t.primary_key.length > 0 && t.primary_key.every((pk) => has(cols, pk))) return true;
  return t.indexes.some((ix) => ix.unique && ix.columns.length === cols.length && ix.columns.every((c) => has(cols, c)));
}

export const NAME_RE = /^[a-z][a-z0-9_]{0,62}$/;
export const IDENT_RE = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
