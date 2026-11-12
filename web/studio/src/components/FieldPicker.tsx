import { useState } from "react";
import type { Column, Field } from "../types";
import { eqi, IDENT_RE, lc } from "../util";

/**
 * Checklist of a table's columns. Checked columns become document fields; each can be
 * renamed. Fields keep the table's column order.
 */
export default function FieldPicker({
  columns,
  fields,
  onChange,
  pk = [],
  joined = [],
}: {
  columns: Column[];
  fields: Field[];
  onChange: (f: Field[]) => void;
  pk?: string[];
  joined?: string[]; // join columns, marked so users see why they may be redundant
}) {
  const [filter, setFilter] = useState("");
  const sel = new Map(fields.map((f) => [lc(f.column), f]));
  const known = new Set(columns.map((c) => lc(c.name)));
  const missing = fields.filter((f) => !known.has(lc(f.column)));

  const ordered = (next: Map<string, Field>): Field[] => [
    ...columns.filter((c) => next.has(lc(c.name))).map((c) => next.get(lc(c.name))!),
    ...missing.filter((f) => next.has(lc(f.column))),
  ];
  const toggle = (c: Column) => {
    const next = new Map(sel);
    if (next.has(lc(c.name))) next.delete(lc(c.name));
    else next.set(lc(c.name), { column: c.name });
    onChange(ordered(next));
  };
  const rename = (column: string, as: string) => {
    const next = new Map(sel);
    const f = next.get(lc(column));
    if (!f) return;
    next.set(lc(column), as && as !== f.column ? { column: f.column, as } : { column: f.column });
    onChange(ordered(next));
  };
  const remove = (column: string) => {
    const next = new Map(sel);
    next.delete(lc(column));
    onChange(ordered(next));
  };

  const f = filter.trim().toLowerCase();
  const visible = f ? columns.filter((c) => c.name.toLowerCase().includes(f)) : columns;

  return (
    <div className="fields">
      <div className="fields-tools">
        {columns.length > 10 && (
          <input
            className="input small"
            placeholder="Filter columns"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label="Filter columns"
          />
        )}
        <span className="muted small">
          {fields.length} of {columns.length} columns selected
        </span>
        <span className="spacer" />
        <button className="link" onClick={() => onChange(columns.map((c) => sel.get(lc(c.name)) ?? { column: c.name }))}>
          All
        </button>
        <button className="link" onClick={() => onChange([])}>
          None
        </button>
      </div>
      <div className="field-rows">
        {visible.map((c) => {
          const fld = sel.get(lc(c.name));
          const bad = fld?.as !== undefined && fld.as !== "" && !IDENT_RE.test(fld.as);
          return (
            <div key={c.name} className={"field-row" + (fld ? " on" : "")}>
              <label className="field-name">
                <input type="checkbox" checked={!!fld} onChange={() => toggle(c)} />
                <span className="mono">{c.name}</span>
                {pk.some((k) => eqi(k, c.name)) && <span className="badge">PK</span>}
                {joined.some((k) => eqi(k, c.name)) && (
                  <span className="badge" title="Used to join">
                    join
                  </span>
                )}
              </label>
              <span className="type mono">{c.type}</span>
              {fld ? (
                <span className="rename">
                  <span className="arrow" aria-hidden="true">
                    →
                  </span>
                  <input
                    className={"input small mono" + (bad ? " invalid" : "")}
                    value={fld.as ?? ""}
                    placeholder={c.name}
                    onChange={(e) => rename(c.name, e.target.value.trim())}
                    aria-label={`Field name for ${c.name}`}
                    title={bad ? "Use letters, digits and _; start with a letter or _" : "Field name in the document"}
                  />
                </span>
              ) : (
                <span />
              )}
            </div>
          );
        })}
        {missing.map((m) => (
          <div key={"missing-" + m.column} className="field-row on missing">
            <span className="field-name">
              <span className="mono">{m.column}</span>
              <span className="badge bad">missing</span>
            </span>
            <span className="type">column no longer exists</span>
            <button className="link" onClick={() => remove(m.column)}>
              Remove
            </button>
          </div>
        ))}
        {visible.length === 0 && <div className="muted small pad-s">No columns match.</div>}
      </div>
    </div>
  );
}
