import { useRef, useState } from "react";
import { loadTable, useAsync, useSchemaVersion } from "../../hooks";
import type { Calc, Design, Relation, Table, TableDetail } from "../../types";
import { eqi, outputName, uniqueName } from "../../util";
import { calcErrors, topLevelNames } from "./model";

const FUNCTIONS: { sig: string; help: string }[] = [
  { sig: "IF(condition, then, else)", help: "Pick a value by a condition" },
  { sig: "ROUND(number, digits)", help: "Round to a number of decimals" },
  { sig: "COALESCE(value, fallback)", help: "The first value that is not empty" },
  { sig: "UPPER(text)", help: "Text in capitals" },
  { sig: "LOWER(text)", help: "Text in lower case" },
  { sig: "TRIM(text)", help: "Remove spaces at both ends" },
  { sig: "LEFT(text, count)", help: "The first characters" },
  { sig: "REPLACE(text, find, replace_with)", help: "Replace part of a text" },
  { sig: "CONTAINS(text, part)", help: "True when the text contains part" },
  { sig: 'DATEDIFF("day", start, end)', help: "Days (or months, years…) between two dates" },
  { sig: "YEAR(date)", help: "The year of a date" },
  { sig: "ISBLANK(value)", help: "True when empty" },
  { sig: "NUMBER(text)", help: "Text as a number (empty when it is not one)" },
  { sig: "TEXT(value)", help: "A value as text" },
];

/** Columns worth summarising: not keys, and not the columns the relation joins on. */
function valueColumns(t: Table, r: Relation) {
  const skip = [...t.primary_key, ...r.join.map((j) => j.to), ...r.join.map((j) => j.from)];
  return t.columns.filter((c) => !skip.some((k) => eqi(k, c.name)));
}

function numericColumn(t: Table, r: Relation): string | undefined {
  const cols = valueColumns(t, r);
  return (
    cols.find((c) => /decimal|numeric|money|float|real/i.test(c.type))?.name ?? cols.find((c) => /int/i.test(c.type))?.name
  );
}

/** Calculated fields: formulas over the root, its lookups and summaries of its lists. */
export default function CalcCard({ design, onChange, errors }: { design: Design; onChange: (d: Design) => void; errors: string[] }) {
  const calcs = design.calculated ?? [];
  const v = useSchemaVersion();
  const tablesKey = [design.root.table, ...design.related.map((r) => r.table)].join("|");
  const tables = useAsync(() => Promise.all(design.related.map((r) => loadTable(r.table))), [tablesKey, v], true);
  const [focus, setFocus] = useState(-1);
  const [help, setHelp] = useState(false);
  const inputs = useRef<(HTMLTextAreaElement | null)[]>([]);
  const errs = calcErrors(errors);

  const setCalcs = (next: Calc[]) => onChange({ ...design, calculated: next });
  const set = (i: number, patch: Partial<Calc>) => setCalcs(calcs.map((c, j) => (j === i ? { ...c, ...patch } : c)));
  function add(expr = "") {
    setCalcs([...calcs, { name: uniqueName("new_field", topLevelNames(design)), expr }]);
    setFocus(calcs.length);
    requestAnimationFrame(() => inputs.current[calcs.length]?.focus());
  }
  function insert(text: string) {
    const i = focus >= 0 && focus < calcs.length ? focus : calcs.length - 1;
    if (i < 0) {
      add(text);
      return;
    }
    const el = inputs.current[i];
    const expr = calcs[i].expr;
    const start = el?.selectionStart ?? expr.length;
    const end = el?.selectionEnd ?? expr.length;
    set(i, { expr: expr.slice(0, start) + text + expr.slice(end) });
    requestAnimationFrame(() => {
      el?.focus();
      el?.setSelectionRange(start + text.length, start + text.length);
    });
  }

  // What formulas can use: document fields, lookup columns and list summaries.
  const fields = [
    ...design.root.fields.map(outputName),
    ...design.related.filter((r) => r.shape === "fields" && !r.via && !r.hidden && !r.path).flatMap((r) => r.fields.map(outputName)),
  ];
  const details = new Map<string, TableDetail>();
  tables.data?.forEach((d, i) => details.set(design.related[i]?.id ?? "", d));
  const lookups = design.related.filter((r) => r.shape === "fields" && !r.via);
  const lists = design.related.filter((r) => r.shape === "list" || r.via);

  return (
    <section className="card calc-card">
      <header className="card-head">
        <div>
          <div className="eyebrow">Calculated fields</div>
          <h3>Fields computed with formulas</h3>
          <div className="muted small">
            Combine columns from the root and its lookups, or summarise lists, for example{" "}
            <span className="mono">ROUND(SUM(rates.amount) / carat, 2)</span>.
          </div>
        </div>
        <button className="btn small" onClick={() => add()}>
          + Add calculated field
        </button>
      </header>

      {calcs.length > 0 && (
        <div className="calc-rows">
          {calcs.map((c, i) => {
            const problems = errs.get(c.name) ?? [];
            const nameBad = !c.name || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(c.name);
            return (
              <div key={i} className={"calc-row" + (problems.length ? " bad" : "")}>
                <input
                  className={"input mono calc-name" + (nameBad ? " invalid" : "")}
                  value={c.name}
                  aria-label="Field name"
                  placeholder="field_name"
                  onChange={(e) => set(i, { name: e.target.value.trim() })}
                  title={nameBad ? "Use letters, digits and _; start with a letter or _" : "Field name in the document"}
                />
                <span className="calc-eq" aria-hidden="true">
                  =
                </span>
                <textarea
                  ref={(el) => {
                    inputs.current[i] = el;
                  }}
                  className="input mono calc-expr"
                  rows={Math.min(6, Math.max(1, c.expr.split("\n").length))}
                  value={c.expr}
                  spellCheck={false}
                  placeholder="formula, for example COUNT(inclusions)"
                  aria-label={`Formula for ${c.name || "the new field"}`}
                  onFocus={() => setFocus(i)}
                  onChange={(e) => set(i, { expr: e.target.value })}
                />
                <button className="btn small ghost" onClick={() => setCalcs(calcs.filter((_, j) => j !== i))} aria-label={`Remove ${c.name}`}>
                  Remove
                </button>
                {problems.map((p, k) => (
                  <div key={k} className="calc-error error-text small" role="alert">
                    {p}
                  </div>
                ))}
              </div>
            );
          })}
        </div>
      )}

      <div className="palette">
        <div className="palette-head">
          <span className="muted small">Insert into the formula:</span>
          <button className="link" onClick={() => setHelp(!help)} aria-expanded={help}>
            {help ? "Hide syntax" : "Syntax"}
          </button>
        </div>
        {help && (
          <ul className="syntax small">
            <li>
              Fields by name: <span className="mono">carat</span>; columns of a lookup: <span className="mono">lab.country</span>.
            </li>
            <li>
              Text in quotes: <span className="mono">"sold"</span>; join text with <span className="mono">&amp;</span> (an empty value
              joins as empty text; use <span className="mono">COALESCE(lab.country, "?")</span> for a placeholder). Dates as{" "}
              <span className="mono">"2024-12-31"</span>.
            </li>
            <li>
              Operators: <span className="mono">+ - * / %</span>, <span className="mono">= &lt;&gt; &lt; &lt;= &gt; &gt;=</span>,{" "}
              <span className="mono">AND OR NOT</span>, <span className="mono">IN ("a", "b")</span>. Dividing by zero gives an empty value.
            </li>
            <li>
              Lists are summarised: <span className="mono">COUNT(rates)</span>, <span className="mono">SUM(rates.amount)</span>,{" "}
              <span className="mono">AVG</span>, <span className="mono">MIN</span>, <span className="mono">MAX</span>,{" "}
              <span className="mono">COUNTIF(inclusions.severity &gt;= 3)</span>.
            </li>
            <li>Calculated fields can use each other. The current date is not available: documents rebuild only when their data changes.</li>
          </ul>
        )}
        {fields.length > 0 && (
          <div className="chips">
            <span className="chips-label">Fields</span>
            {fields.map((f) => (
              <button key={f} className="chip-btn mono" onClick={() => insert(f)}>
                {f}
              </button>
            ))}
          </div>
        )}
        {lookups.map((r) => {
          const t = details.get(r.id)?.table;
          if (!t) return null;
          return (
            <div key={r.id} className="chips">
              <span className="chips-label">{r.id}</span>
              {t.columns.map((c) => (
                <button key={c.name} className="chip-btn mono" onClick={() => insert(`${r.id}.${c.name}`)} title={c.type}>
                  {r.id}.{c.name}
                </button>
              ))}
            </div>
          );
        })}
        {lists.map((r) => {
          const t = details.get(r.id)?.table;
          const num = t ? numericColumn(t, r) : undefined;
          const col = num ?? (t ? valueColumns(t, r)[0]?.name : undefined) ?? "column";
          return (
            <div key={r.id} className="chips">
              <span className="chips-label">
                {r.id}
                {r.hidden ? " (hidden)" : ""}
              </span>
              <button className="chip-btn mono" onClick={() => insert(`COUNT(${r.id})`)}>
                COUNT({r.id})
              </button>
              {num && (
                <>
                  <button className="chip-btn mono" onClick={() => insert(`SUM(${r.id}.${num})`)}>
                    SUM({r.id}.{num})
                  </button>
                  <button className="chip-btn mono" onClick={() => insert(`AVG(${r.id}.${num})`)}>
                    AVG({r.id}.{num})
                  </button>
                </>
              )}
              <button className="chip-btn mono" onClick={() => insert(`MAX(${r.id}.${col})`)}>
                MAX({r.id}.{col})
              </button>
              <button className="chip-btn mono" onClick={() => insert(`COUNTIF(${r.id}.${col} > 0)`)}>
                COUNTIF(…)
              </button>
            </div>
          );
        })}
        <div className="chips">
          <span className="chips-label">Functions</span>
          {FUNCTIONS.map((f) => (
            <button key={f.sig} className="chip-btn mono" title={`${f.sig}: ${f.help}`} onClick={() => insert(f.sig.slice(0, f.sig.indexOf("(") + 1))}>
              {f.sig.slice(0, f.sig.indexOf("("))}
            </button>
          ))}
        </div>
        {lists.length === 0 && (
          <p className="hint">
            Add a list of related rows to summarise it here. Turn on “Use only in calculated fields” on a list to count or total it
            without putting its rows in documents.
          </p>
        )}
      </div>
    </section>
  );
}
