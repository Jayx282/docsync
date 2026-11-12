import { useCallback, useEffect, useRef, useState } from "react";
import { api, message } from "../../api";
import type { Design, Finding, PreviewDoc, PreviewResult, SchemaField, SchemaType, ValidateResult } from "../../types";
import { fmtBytes } from "../../util";
import { Code, Empty, ErrorBox, Spinner } from "../../components/ui";
import { pretty } from "../../components/highlight";
import { designKey } from "./model";

type Tab = "preview" | "shape" | "yaml" | "sql" | "checks";

const TABS: { id: Tab; label: string }[] = [
  { id: "preview", label: "Preview" },
  { id: "shape", label: "Shape" },
  { id: "yaml", label: "Config" },
  { id: "sql", label: "SQL" },
  { id: "checks", label: "Checks" },
];

/** Live results next to the editor: documents from real data, schema, config, SQL, checks. */
export default function Results({ design, onPreview }: { design: Design; onPreview?: (r: PreviewResult) => void }) {
  const [tab, setTab] = useState<Tab>("preview");
  const [ids, setIds] = useState("");
  const [auto, setAuto] = useState(true);
  const [res, setRes] = useState<PreviewResult | null>(null);
  const [resKey, setResKey] = useState("");
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string>();
  const [checks, setChecks] = useState<{ key: string; res: ValidateResult } | null>(null);
  const [checking, setChecking] = useState(false);
  const seq = useRef(0);
  const latest = useRef({ design, ids, onPreview });
  latest.current = { design, ids, onPreview };
  const key = designKey(design);

  const run = useCallback(async () => {
    const { design, ids } = latest.current;
    if (!design.root.table) return;
    const n = ++seq.current;
    setLoading(true);
    setErr(undefined);
    try {
      const r = await api.preview(design, ids.split(/[\s,]+/).filter(Boolean), 3);
      if (n === seq.current) {
        setRes(r);
        setResKey(designKey(design));
        latest.current.onPreview?.(r);
      }
    } catch (e) {
      if (n === seq.current) setErr(message(e));
    } finally {
      if (n === seq.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!auto) return;
    const t = setTimeout(run, 450);
    return () => clearTimeout(t);
  }, [key, auto, run]);

  async function check() {
    setChecking(true);
    try {
      setChecks({ key, res: await api.validate(design) });
    } catch (e) {
      setChecks({ key, res: { ok: false, errors: [message(e)], findings: [], script: "" } });
    } finally {
      setChecking(false);
    }
  }

  const stale = res !== null && resKey !== key;
  return (
    <aside className="results" aria-label="Results">
      <div className="results-tabs" role="tablist">
        {TABS.map((t) => (
          <button key={t.id} role="tab" aria-selected={tab === t.id} className={tab === t.id ? "on" : ""} onClick={() => setTab(t.id)}>
            {t.label}
            {t.id === "preview" && res && res.errors.length > 0 && <span className="dot bad" />}
            {t.id === "checks" && checks && !checks.res.ok && <span className="dot bad" />}
          </button>
        ))}
        <span className="spacer" />
        {loading && <Spinner small />}
      </div>
      <div className="results-body">
        {tab === "preview" && (
          <>
            <div className="preview-tools">
              <input
                className="input small mono"
                placeholder="ids to preview, e.g. 1001, 1002 (empty: first 3)"
                value={ids}
                aria-label="Root ids to preview"
                onChange={(e) => setIds(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && void run()}
              />
              <button className="btn small" onClick={() => void run()}>
                Run
              </button>
              <label className="check small" title="Rebuild the preview after every change">
                <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} /> Auto
              </label>
            </div>
            {err && <ErrorBox errors={err} />}
            {!res && !err && loading && <Empty title="Building documents…" />}
            {res && res.errors.length > 0 && <ErrorBox errors={res.errors} title="This design cannot be built yet" />}
            {res && (
              <div className={stale ? "stale" : ""}>
                {res.documents.map((d, i) => (
                  <DocCard key={d.root + ":" + i} doc={d} keyName={design.root.key} open={i === 0} />
                ))}
                {res.documents.length > 0 && (
                  <div className="faint small pad-s">
                    {res.documents.length} {res.documents.length === 1 ? "document" : "documents"} built in {res.took_ms} ms
                    {stale && " · updating…"}
                  </div>
                )}
              </div>
            )}
          </>
        )}
        {tab === "shape" &&
          (res && res.fields.length > 0 ? (
            <>
              <p className="muted small">Columns of the output (Parquet schema). Lists are nested; types come from SQL Server.</p>
              <TypeTree fields={res.fields} />
            </>
          ) : (
            <Empty title="No shape yet">The shape appears once the design builds.</Empty>
          ))}
        {tab === "yaml" &&
          (res?.yaml ? (
            <>
              <p className="muted small">
                The docsync config this design compiles to. Deploying writes it to the pipeline folder; you can also run it
                with <span className="mono">docsync run --config docsync.yaml</span>.
              </p>
              <Code text={res.yaml} lang="yaml" copy downloadAs="docsync.yaml" />
            </>
          ) : (
            <Empty title="No config yet">Fix the design errors shown in Preview.</Empty>
          ))}
        {tab === "sql" &&
          (res && res.sql.length > 0 ? (
            <>
              <p className="muted small">
                One query per part of the document. <span className="mono">{"{{roots}}"}</span> is the temporary table of
                changed ids that docsync fills on every cycle.
              </p>
              {res.sql.map((f) => (
                <div key={f.name} className="fragment">
                  <div className="fragment-head">
                    <b className="mono">{f.name}</b>
                    <span className="chip">{f.cardinality === "many" ? `list · ${f.path}` : f.path ? `object · ${f.path}` : "fields"}</span>
                  </div>
                  <Code text={f.sql} lang="sql" copy />
                </div>
              ))}
            </>
          ) : (
            <Empty title="No SQL yet">Fix the design errors shown in Preview.</Empty>
          ))}
        {tab === "checks" && (
          <ChecksTab checks={checks} stale={!!checks && checks.key !== key} checking={checking} onRun={check} />
        )}
      </div>
    </aside>
  );
}

function DocCard({ doc, keyName, open }: { doc: PreviewDoc; keyName: string; open: boolean }) {
  const [isOpen, setOpen] = useState(open);
  return (
    <div className={"doc" + (doc.error ? " bad" : "")}>
      <button className="doc-head" onClick={() => setOpen(!isOpen)} aria-expanded={isOpen}>
        <span>{isOpen ? "▾" : "▸"}</span>
        <span className="mono">
          {keyName} = {doc.root}
        </span>
        <span className="spacer" />
        {doc.error ? <span className="chip bad">error</span> : <span className="faint small">{fmtBytes(doc.bytes)}</span>}
      </button>
      {isOpen && (doc.error ? <ErrorBox errors={doc.error} /> : <Code text={pretty(doc.json ?? "")} lang="json" copy />)}
    </div>
  );
}

function typeLabel(t: SchemaType): string {
  switch (t.kind) {
    case "decimal":
      return `decimal(${t.precision ?? 0},${t.scale ?? 0})`;
    case "list":
      return t.elem ? `list<${t.elem.kind === "struct" ? "struct" : typeLabel(t.elem)}>` : "list";
    default:
      return t.kind;
  }
}

function children(t: SchemaType): SchemaField[] | undefined {
  if (t.kind === "struct") return t.fields;
  if (t.kind === "list" && t.elem?.kind === "struct") return t.elem.fields;
  return undefined;
}

function TypeTree({ fields }: { fields: SchemaField[] }) {
  return (
    <ul className="type-tree">
      {fields.map((f) => {
        const kids = children(f.type);
        return (
          <li key={f.name}>
            <span className="mono">{f.name}</span> <span className="type mono">{typeLabel(f.type)}</span>
            {f.type.sql_type && <span className="faint small mono"> {f.type.sql_type}</span>}
            {kids && <TypeTree fields={kids} />}
          </li>
        );
      })}
    </ul>
  );
}

function ChecksTab({
  checks,
  stale,
  checking,
  onRun,
}: {
  checks: { key: string; res: ValidateResult } | null;
  stale: boolean;
  checking: boolean;
  onRun: () => void;
}) {
  return (
    <>
      <div className="preview-tools">
        <p className="muted small grow">
          Checks what the database needs before this design can be deployed: change tracking, snapshot isolation,
          primary keys, indexes and permissions.
        </p>
        <button className="btn small" onClick={onRun} disabled={checking}>
          {checking ? <Spinner small /> : null} {checks ? "Run again" : "Run checks"}
        </button>
      </div>
      {stale && <p className="warn-text small">The design changed since these checks ran.</p>}
      {checks && (
        <>
          {checks.res.errors.length > 0 && <ErrorBox errors={checks.res.errors} title="Design errors" />}
          <FindingList findings={checks.res.findings} />
          {checks.res.script && (
            <>
              <h4>SQL for your DBA</h4>
              <p className="muted small">Review it, rename docsync_login to the login docsync will use, then run it on the database.</p>
              <Code text={checks.res.script} lang="sql" copy downloadAs="docsync-setup.sql" />
            </>
          )}
        </>
      )}
    </>
  );
}

export function FindingList({ findings, only }: { findings: Finding[]; only?: Finding["severity"][] }) {
  const order = { FAIL: 0, warn: 1, ok: 2 } as const;
  const list = findings.filter((f) => !only || only.includes(f.severity)).sort((a, b) => order[a.severity] - order[b.severity]);
  if (list.length === 0) return null;
  return (
    <ul className="findings">
      {list.map((f, i) => (
        <li key={i} className={"finding " + (f.severity === "FAIL" ? "bad" : f.severity)}>
          <span className="sev">{f.severity === "FAIL" ? "✕" : f.severity === "warn" ? "!" : "✓"}</span>
          <div>
            <b>{f.check}</b>
            <div className="small">{f.detail}</div>
          </div>
        </li>
      ))}
    </ul>
  );
}
