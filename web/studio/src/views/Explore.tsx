import { useMemo, useState } from "react";
import { message } from "../api";
import { go, href, useTable, useTables } from "../hooks";
import type { Connection, TableDetail, TableRow } from "../types";
import { eqi, fmtCompact, fmtInt, schemaOf, shortName } from "../util";
import { ErrorBox, Spinner } from "../components/ui";
import TableGraph from "../components/TableGraph";
import { createDesignFor } from "./design/model";

export default function Explore({ table, conn }: { table?: string; conn: Connection | null }) {
  const tables = useTables();
  const [filter, setFilter] = useState("");
  const list = useMemo(() => {
    const all = tables.data ?? [];
    const f = filter.trim().toLowerCase();
    return f ? all.filter((t) => t.name.toLowerCase().includes(f)) : all;
  }, [tables.data, filter]);
  const shown = list.slice(0, 500);

  return (
    <div className="split">
      <aside className="sidebar">
        <div className="sidebar-head">
          <input
            className="input"
            placeholder={tables.data ? `Search ${tables.data.length} tables` : "Search tables"}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label="Search tables"
          />
        </div>
        <div className="item-list" role="list">
          {tables.loading && (
            <div className="pad">
              <Spinner />
            </div>
          )}
          {tables.error && (
            <div className="pad">
              <ErrorBox errors={tables.error} />
            </div>
          )}
          {shown.map((t) => (
            <a
              key={t.name}
              role="listitem"
              href={href(`explore/${t.name}`)}
              className={"item" + (table && eqi(table, t.name) ? " on" : "")}
            >
              <span className="item-name">
                <span className="faint">{schemaOf(t.name)}.</span>
                {shortName(t.name)}
              </span>
              <span className="item-meta">
                {fmtCompact(t.rows)}
                <span className={"dot " + (t.change_tracking ? "ok" : "off")} title={t.change_tracking ? "Change tracking on" : "Change tracking off"} />
              </span>
            </a>
          ))}
          {list.length > shown.length && (
            <div className="muted small pad">{list.length - shown.length} more; refine the search.</div>
          )}
          {tables.data && list.length === 0 && <div className="muted small pad">No tables match.</div>}
        </div>
      </aside>
      <section className="content">
        {table ? <TableView key={table} name={table} /> : <ExploreHome tables={tables.data} conn={conn} />}
      </section>
    </div>
  );
}

function ExploreHome({ tables, conn }: { tables?: TableRow[]; conn: Connection | null }) {
  const total = tables?.reduce((n, t) => n + t.rows, 0) ?? 0;
  const tracked = tables?.filter((t) => t.change_tracking).length ?? 0;
  const roots = tables?.filter((t) => t.primary_key.length === 1).length ?? 0;
  return (
    <div className="home">
      <div className="page-head">
        <div>
          <div className="eyebrow">{conn?.server ?? "SQL Server"}</div>
          <h1>{conn?.database ?? "Database"}</h1>
        </div>
      </div>
      {tables && (
        <div className="stats">
          <Stat label="tables" value={fmtInt(tables.length)} />
          <Stat label="rows" value={fmtCompact(total)} />
          <Stat label="with change tracking" value={`${tracked} of ${tables.length}`} />
          <Stat label="can be a document root" value={fmtInt(roots)} hint="single-column primary key" />
        </div>
      )}
      <div className="card steps">
        <h3>Build a search index in four steps</h3>
        <ol>
          <li>
            <b>Explore.</b> Pick a table on the left to see its columns, keys and the tables around it.
          </li>
          <li>
            <b>Design.</b> Choose the table whose rows become documents, then add related data as fields or nested
            lists. Suggestions come from foreign keys and matching column names.
          </li>
          <li>
            <b>Preview.</b> Every change shows real documents built from your data, the generated SQL and the docsync
            config.
          </li>
          <li>
            <b>Deploy.</b> Checks list what the database needs (change tracking, indexes, permissions), then the
            pipeline starts and keeps documents up to date.
          </li>
        </ol>
      </div>
    </div>
  );
}

function Stat({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="stat" title={hint}>
      <div className="stat-value">{value}</div>
      <div className="stat-label">{label}</div>
    </div>
  );
}

function TableView({ name }: { name: string }) {
  const t = useTable(name);
  const [creating, setCreating] = useState(false);
  const [err, setErr] = useState<string>();

  if (t.loading)
    return (
      <div className="pad">
        <Spinner />
      </div>
    );
  if (t.error || !t.data) return <ErrorBox errors={t.error ?? "Table not found"} />;
  const d = t.data;
  const tb = d.table;
  const canRoot = tb.primary_key.length === 1;

  async function design() {
    setCreating(true);
    setErr(undefined);
    try {
      const n = await createDesignFor(tb.name);
      go(`design/${n}`);
    } catch (e) {
      setErr(message(e));
      setCreating(false);
    }
  }

  return (
    <div className="table-view">
      <div className="page-head">
        <div>
          <div className="eyebrow">{schemaOf(tb.name)}</div>
          <h1>{shortName(tb.name)}</h1>
          <div className="meta-row">
            <span>{fmtInt(tb.rows)} rows</span>
            <span>{tb.columns.length} columns</span>
            <span>
              key{" "}
              {tb.primary_key.length ? (
                <span className="mono">{tb.primary_key.join(", ")}</span>
              ) : (
                <span className="warn-text">none</span>
              )}
            </span>
            {tb.change_tracking ? <span className="chip ok">change tracking on</span> : <span className="chip">change tracking off</span>}
          </div>
        </div>
        <div className="actions">
          <button
            className="btn primary"
            disabled={!canRoot || creating}
            onClick={design}
            title={canRoot ? "Start a design with one document per row of this table" : "A document root needs a single-column primary key"}
          >
            {creating && <Spinner small />} Design documents from this table
          </button>
        </div>
      </div>
      {err && <ErrorBox errors={err} />}
      <section className="card flush">
        <TableGraph detail={d} />
      </section>
      <div className="grid-2">
        <ColumnsCard detail={d} />
        <div className="stack">
          <RelatedCard detail={d} />
          <KeysCard detail={d} />
        </div>
      </div>
    </div>
  );
}

function ColumnsCard({ detail }: { detail: TableDetail }) {
  const tb = detail.table;
  const fkCols = new Set(detail.references.flatMap((fk) => fk.from_columns.map((c) => c.toLowerCase())));
  return (
    <section className="card">
      <h3>Columns</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Null</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {tb.columns.map((c) => (
            <tr key={c.name}>
              <td className="mono">{c.name}</td>
              <td className="mono muted">{c.type}</td>
              <td className="muted">{c.nullable ? "yes" : ""}</td>
              <td className="badges">
                {tb.primary_key.some((k) => eqi(k, c.name)) && <span className="badge">PK</span>}
                {fkCols.has(c.name.toLowerCase()) && <span className="badge">FK</span>}
                {c.identity && <span className="badge">identity</span>}
                {c.computed && <span className="badge">computed</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function RelatedCard({ detail }: { detail: TableDetail }) {
  if (detail.table.primary_key.length !== 1) {
    return (
      <section className="card">
        <h3>Related data</h3>
        <p className="muted small">
          Suggestions appear for tables with a single-column primary key, because only those can be document roots.
        </p>
      </section>
    );
  }
  return (
    <section className="card">
      <h3>Related data for documents of this table</h3>
      {detail.related.length === 0 && <p className="muted small">Nothing found from foreign keys or column names.</p>}
      <ul className="plain-list">
        {detail.related.map((s) => (
          <li key={s.table + (s.via?.table ?? "")}>
            <span className={"shape-icon " + s.shape}>{s.shape === "list" ? "[ ]" : "{ }"}</span>
            <a href={href(`explore/${s.table}`)} className="mono">
              {shortName(s.table)}
            </a>
            {s.via && <span className="muted"> via {shortName(s.via.table)}</span>}
            <span className={"chip " + (s.guessed ? "guess" : "ok")}>{s.guessed ? "guessed" : "foreign key"}</span>
            <div className="muted small">{s.reason}</div>
          </li>
        ))}
      </ul>
    </section>
  );
}

function KeysCard({ detail }: { detail: TableDetail }) {
  const tb = detail.table;
  return (
    <section className="card">
      <h3>Indexes</h3>
      {tb.indexes.length === 0 && <p className="muted small">No indexes.</p>}
      <ul className="plain-list">
        {tb.indexes.map((ix) => (
          <li key={ix.name}>
            <span className="mono">{ix.name}</span> <span className="muted">({ix.columns.join(", ")})</span>{" "}
            {ix.primary ? <span className="badge">primary</span> : ix.unique ? <span className="badge">unique</span> : null}
          </li>
        ))}
      </ul>
      {(detail.references.length > 0 || detail.referenced_by.length > 0) && <h3 className="mt">Foreign keys</h3>}
      <ul className="plain-list">
        {detail.references.map((fk) => (
          <li key={"o" + fk.name}>
            <span className="mono">{fk.from_columns.join(", ")}</span> → <a href={href(`explore/${fk.to_table}`)} className="mono">{fk.to_table}</a>
            <span className="muted">({fk.to_columns.join(", ")})</span>
          </li>
        ))}
        {detail.referenced_by.map((fk) => (
          <li key={"i" + fk.name}>
            <a href={href(`explore/${fk.from_table}`)} className="mono">{fk.from_table}</a>
            <span className="muted">({fk.from_columns.join(", ")})</span> → <span className="mono">{fk.to_columns.join(", ")}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
