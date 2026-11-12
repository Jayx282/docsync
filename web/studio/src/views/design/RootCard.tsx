import { useState } from "react";
import { api, message } from "../../api";
import { href, loadTable, useTable, useTables } from "../../hooks";
import type { Design, FilterCount } from "../../types";
import { fmtInt, shortName } from "../../util";
import { Combobox, ErrorBox, Spinner } from "../../components/ui";
import FieldPicker from "../../components/FieldPicker";
import { changeRoot, filterErrors, rootOptions } from "./model";

export default function RootCard({ design, onChange, errors = [] }: { design: Design; onChange: (d: Design) => void; errors?: string[] }) {
  const tables = useTables();
  const t = useTable(design.root.table || undefined);
  const [changing, setChanging] = useState(!design.root.table);
  const [err, setErr] = useState<string>();
  const [count, setCount] = useState<{ filter: string; res: FilterCount } | null>(null);
  const [counting, setCounting] = useState(false);
  const filter = design.root.filter ?? "";
  const filterProblems = filterErrors(errors);

  async function countRows() {
    setCounting(true);
    try {
      setCount({ filter, res: await api.filterCount(design) });
    } catch (e) {
      setCount({ filter, res: { errors: [message(e)], total: 0, matching: 0, took_ms: 0 } });
    } finally {
      setCounting(false);
    }
  }

  async function pick(name: string) {
    if (design.related.length > 0 && !window.confirm("Changing the root table removes all related data from this design. Continue?")) return;
    setErr(undefined);
    try {
      const detail = await loadTable(name);
      onChange(changeRoot(design, detail.table));
      setChanging(false);
    } catch (e) {
      setErr(message(e));
    }
  }

  const table = t.data?.table;
  return (
    <section className="card root-card">
      <header className="card-head">
        <div>
          <div className="eyebrow">Document root</div>
          <h3>
            {design.root.table ? (
              <>
                One document per <a className="mono" href={href(`explore/${design.root.table}`)}>{shortName(design.root.table)}</a> row
              </>
            ) : (
              "Choose the table whose rows become documents"
            )}
          </h3>
          {table && (
            <div className="muted small">
              {fmtInt(table.rows)} documents · identified by <span className="mono">{design.root.key}</span>
              {!table.change_tracking && <span className="chip warn">change tracking off</span>}
            </div>
          )}
        </div>
        {!changing && design.root.table && (
          <button className="btn small" onClick={() => setChanging(true)}>
            Change
          </button>
        )}
      </header>
      {changing && (
        <div className="root-pick">
          <Combobox
            value={design.root.table}
            options={rootOptions(tables.data)}
            onChange={pick}
            placeholder="Search tables"
            autoFocus={!design.root.table}
            ariaLabel="Root table"
          />
          {design.root.table && (
            <button className="btn small ghost" onClick={() => setChanging(false)}>
              Cancel
            </button>
          )}
        </div>
      )}
      {err && <ErrorBox errors={err} />}
      {t.loading && <Spinner />}
      {t.error && <ErrorBox errors={t.error} />}
      {table && (
        <FieldPicker
          columns={table.columns}
          fields={design.root.fields}
          pk={table.primary_key}
          onChange={(fields) => onChange({ ...design, root: { ...design.root, fields } })}
        />
      )}
      {table && (
        <div className="root-filter">
          <label htmlFor="root-filter">Only rows where</label>
          <div className="filter-input">
            <input
              id="root-filter"
              className={"input mono" + (filterProblems.length ? " invalid" : "")}
              value={filter}
              spellCheck={false}
              placeholder='optional, for example: status <> "sold"'
              onChange={(e) => {
                onChange({ ...design, root: { ...design.root, filter: e.target.value } });
                setCount(null);
              }}
            />
            <button className="btn small" onClick={countRows} disabled={counting} title="Count the rows that would become documents">
              {counting && <Spinner small />} Count rows
            </button>
          </div>
          {filterProblems.map((p, i) => (
            <div key={i} className="error-text small" role="alert">
              {p}
            </div>
          ))}
          {count && count.filter === filter && (
            <div className="small muted">
              {count.res.errors.length > 0
                ? count.res.errors.join("; ")
                : `${fmtInt(count.res.matching)} of ${fmtInt(count.res.total)} rows become documents (${count.res.took_ms} ms)`}
            </div>
          )}
          <p className="hint">
            A formula over {shortName(design.root.table)}'s columns. Rows that stop matching leave the index; rows that start
            matching join it.
          </p>
        </div>
      )}
    </section>
  );
}
