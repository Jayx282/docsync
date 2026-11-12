import { useEffect, useState } from "react";
import { api, message } from "../../api";
import { loadTable, useAsync, useSchemaVersion, useTable, useTables } from "../../hooks";
import type { Design, Relation, Shape, Suggestion } from "../../types";
import { eqi, fmtCompact, lc, shortName } from "../../util";
import { Combobox, ErrorBox, Segmented, Spinner } from "../../components/ui";
import type { Option } from "../../components/ui";
import { newRelation, relationFromSuggestion, usedTables } from "./model";

export default function AddRelated({ design, onAdd }: { design: Design; onAdd: (r: Relation) => void }) {
  const v = useSchemaVersion();
  const sugg = useAsync(() => api.suggestions(design.root.table), [design.root.table, v]);
  const [filter, setFilter] = useState("");
  const [custom, setCustom] = useState(false);
  const [adding, setAdding] = useState<string>();
  const [err, setErr] = useState<string>();
  const used = usedTables(design);

  const blocked = (s: Suggestion): string | undefined => {
    if (used.has(lc(s.table))) return `${shortName(s.table)} is already in this design`;
    if (s.via && used.has(lc(s.via.table))) return `${shortName(s.via.table)} is already in this design`;
    return undefined;
  };
  const f = filter.trim().toLowerCase();
  const list = (sugg.data ?? []).filter((s) => !f || `${s.table} ${s.via?.table ?? ""}`.toLowerCase().includes(f));
  const available = list.filter((s) => !blocked(s));
  const unavailable = list.filter((s) => blocked(s));
  const keyOf = (s: Suggestion) => s.table + "|" + (s.via?.table ?? "");

  async function add(s: Suggestion) {
    setAdding(keyOf(s));
    setErr(undefined);
    try {
      const t = await loadTable(s.table);
      onAdd(relationFromSuggestion(design, s, t.table));
    } catch (e) {
      setErr(message(e));
    } finally {
      setAdding(undefined);
    }
  }

  return (
    <section className="card add-related">
      <header className="card-head">
        <div>
          <div className="eyebrow">Add related data</div>
          <h3>Tables connected to {shortName(design.root.table)}</h3>
          <div className="muted small">From foreign keys and from columns named like a key. Check guessed ones before adding.</div>
        </div>
        <button className="btn small" onClick={() => setCustom(!custom)} aria-expanded={custom}>
          {custom ? "Close" : "Custom join…"}
        </button>
      </header>
      {custom && (
        <CustomRelation
          design={design}
          onAdd={(r) => {
            onAdd(r);
            setCustom(false);
          }}
        />
      )}
      {(sugg.data?.length ?? 0) > 6 && (
        <input className="input small" placeholder="Filter suggestions" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter suggestions" />
      )}
      {sugg.loading && <Spinner />}
      {sugg.error && <ErrorBox errors={sugg.error} />}
      {err && <ErrorBox errors={err} />}
      <div className="suggestions">
        {available.map((s) => (
          <SuggestionRow key={keyOf(s)} s={s} busy={adding === keyOf(s)} onAdd={() => add(s)} />
        ))}
        {unavailable.map((s) => (
          <SuggestionRow key={keyOf(s)} s={s} blocked={blocked(s)} />
        ))}
      </div>
      {sugg.data && sugg.data.length === 0 && (
        <p className="muted small">No connected tables found from foreign keys or column names. Use “Custom join…” to add one.</p>
      )}
      {sugg.data && sugg.data.length > 0 && available.length === 0 && !f && (
        <p className="muted small">Every suggested table is already in this design.</p>
      )}
    </section>
  );
}

function joinText(s: Suggestion): string {
  const j = (pairs: { from: string; to: string }[]) => pairs.map((p) => (eqi(p.from, p.to) ? p.from : `${p.from} = ${p.to}`)).join(", ");
  return s.via ? `${j(s.via.join)} → ${shortName(s.via.table)} → ${j(s.join)}` : j(s.join);
}

function SuggestionRow({ s, onAdd, busy, blocked }: { s: Suggestion; onAdd?: () => void; busy?: boolean; blocked?: string }) {
  return (
    <div className={"suggestion" + (blocked ? " blocked" : "")}>
      <span className={"shape-icon " + s.shape} aria-hidden="true">
        {s.shape === "list" ? "[ ]" : "{ }"}
      </span>
      <div className="s-main">
        <div className="s-title">
          <b>{shortName(s.table)}</b>
          {s.via && <span className="muted"> via {shortName(s.via.table)}</span>}
          <span className={"chip " + (s.guessed ? "guess" : "ok")} title={s.source}>
            {s.guessed ? "guessed" : "foreign key"}
          </span>
          <span className="chip">{s.shape === "list" ? "list" : "fields"}</span>
          <span className="faint small">{fmtCompact(s.rows)} rows</span>
        </div>
        <div className="small">{s.reason}</div>
        <div className="faint small mono">{joinText(s)}</div>
        {blocked && <div className="muted small">{blocked}</div>}
      </div>
      {onAdd && (
        <button className="btn small" disabled={busy} onClick={onAdd}>
          {busy ? <Spinner small /> : "Add"}
        </button>
      )}
    </div>
  );
}

/** Join any table by choosing the columns, optionally through a bridge table. */
function CustomRelation({ design, onAdd }: { design: Design; onAdd: (r: Relation) => void }) {
  const tables = useTables();
  const root = useTable(design.root.table);
  const [table, setTable] = useState("");
  const [viaOn, setViaOn] = useState(false);
  const [via, setVia] = useState("");
  const [shape, setShape] = useState<Shape>("list");
  const [rootCol, setRootCol] = useState(design.root.key);
  const [bridgeRootCol, setBridgeRootCol] = useState("");
  const [bridgeCol, setBridgeCol] = useState("");
  const [relCol, setRelCol] = useState("");
  const [err, setErr] = useState<string>();
  const rel = useTable(table || undefined);
  const br = useTable(viaOn && via ? via : undefined);
  const used = usedTables(design);

  const tableOptions = (exclude?: string): Option[] =>
    (tables.data ?? [])
      .filter((t) => !exclude || !eqi(t.name, exclude))
      .map((t) => ({
        value: t.name,
        hint: `${fmtCompact(t.rows)} rows`,
        disabled: used.has(lc(t.name)) ? "already in this design" : t.primary_key.length === 0 ? "no primary key" : undefined,
      }));
  const cols = (cs?: { name: string }[]) => (cs ?? []).map((c) => c.name);
  const rootCols = cols(root.data?.table.columns);
  const relCols = cols(rel.data?.table.columns);
  const brCols = cols(br.data?.table.columns);

  // Sensible defaults: pick same-named columns when the tables are chosen.
  useEffect(() => {
    if (!rel.data) return;
    const want = viaOn ? bridgeCol : rootCol;
    const c = relCols.find((n) => eqi(n, want)) ?? rel.data.table.primary_key[0] ?? relCols[0] ?? "";
    setRelCol(c);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rel.data]);
  useEffect(() => {
    if (!br.data) return;
    setBridgeRootCol(brCols.find((n) => eqi(n, design.root.key)) ?? brCols[0] ?? "");
    const other = brCols.find((n) => relCols.some((r) => eqi(r, n)) && !eqi(n, design.root.key));
    setBridgeCol(other ?? brCols[1] ?? brCols[0] ?? "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [br.data, rel.data]);

  const ready = !!rel.data && !!relCol && (viaOn ? !!br.data && !!bridgeRootCol && !!bridgeCol && !!rootCol : !!rootCol);

  function add() {
    if (!rel.data) return;
    setErr(undefined);
    try {
      const t = rel.data.table;
      const r = viaOn
        ? newRelation(
            design,
            { table: t.name, shape: "list", join: [{ from: bridgeCol, to: relCol }], via: { table: br.data!.table.name, join: [{ from: rootCol, to: bridgeRootCol }] } },
            t,
          )
        : newRelation(design, { table: t.name, shape, join: [{ from: rootCol, to: relCol }] }, t);
      onAdd(r);
    } catch (e) {
      setErr(message(e));
    }
  }

  const colSelect = (value: string, options: string[], onChange: (v: string) => void, label: string) => (
    <select className="input mono" value={value} onChange={(e) => onChange(e.target.value)} aria-label={label}>
      {!value && <option value="">choose…</option>}
      {options.map((c) => (
        <option key={c} value={c}>
          {c}
        </option>
      ))}
    </select>
  );

  return (
    <div className="custom-rel">
      <div className="form-grid">
        <label>Related table</label>
        <Combobox value={table} options={tableOptions(design.root.table)} onChange={setTable} placeholder="Search tables" ariaLabel="Related table" />
        <span />
        <label className="check">
          <input type="checkbox" checked={viaOn} onChange={(e) => setViaOn(e.target.checked)} /> Through a bridge table (many-to-many)
        </label>
        {viaOn && (
          <>
            <label>Bridge table</label>
            <Combobox value={via} options={tableOptions(table)} onChange={setVia} placeholder="Search tables" ariaLabel="Bridge table" />
          </>
        )}
        <label>Join</label>
        {viaOn ? (
          <div className="join-edit">
            <div>
              <span className="mono muted">{shortName(design.root.table)}.</span>
              {colSelect(rootCol, rootCols, setRootCol, "Root column")} =<span className="mono muted"> {shortName(via) || "bridge"}.</span>
              {colSelect(bridgeRootCol, brCols, setBridgeRootCol, "Bridge column matching the root")}
            </div>
            <div>
              <span className="mono muted">{shortName(via) || "bridge"}.</span>
              {colSelect(bridgeCol, brCols, setBridgeCol, "Bridge column matching the related table")} =
              <span className="mono muted"> {shortName(table) || "related"}.</span>
              {colSelect(relCol, relCols, setRelCol, "Related column")}
            </div>
          </div>
        ) : (
          <div className="join-edit">
            <div>
              <span className="mono muted">{shortName(design.root.table)}.</span>
              {colSelect(rootCol, rootCols, setRootCol, "Root column")} =<span className="mono muted"> {shortName(table) || "related"}.</span>
              {colSelect(relCol, relCols, setRelCol, "Related column")}
            </div>
          </div>
        )}
        {!viaOn && (
          <>
            <label>Show as</label>
            <Segmented
              value={shape}
              options={[
                { value: "fields", label: "Fields (one row)" },
                { value: "list", label: "List (many rows)" },
              ]}
              onChange={setShape}
            />
          </>
        )}
        <span />
        <div>
          <button className="btn primary small" disabled={!ready} onClick={add}>
            Add relation
          </button>
        </div>
      </div>
      {(rel.error || br.error || err) && <ErrorBox errors={[rel.error ?? "", br.error ?? "", err ?? ""]} />}
    </div>
  );
}
