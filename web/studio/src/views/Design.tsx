import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { api, message } from "../api";
import { go, href, loadTable, useAsync, useTables } from "../hooks";
import type { Design, Relation } from "../types";
import { NAME_RE, shortName, toId, uniqueName } from "../util";
import { Combobox, Empty, ErrorBox, Modal, Segmented, Spinner } from "../components/ui";
import { designKey, drafts, newDesign, normalizeDesign, rootOptions } from "./design/model";
import RootCard from "./design/RootCard";
import RelationCard from "./design/RelationCard";
import AddRelated from "./design/AddRelated";
import Results from "./design/Results";
import DeployDialog from "./design/Deploy";
import CalcCard from "./design/CalcCard";

export default function DesignView({ name }: { name?: string }) {
  const [listTick, setListTick] = useState(0);
  const list = useAsync(() => api.designs(), [listTick], true);
  const [showNew, setShowNew] = useState(false);
  useSyncExternalStore(drafts.subscribe, drafts.version);
  const refreshList = useCallback(() => setListTick((t) => t + 1), []);

  return (
    <div className="split">
      <aside className="sidebar narrow">
        <div className="sidebar-head">
          <button className="btn primary block" onClick={() => setShowNew(true)}>
            + New design
          </button>
        </div>
        <div className="item-list" role="list">
          {list.loading && !list.data && (
            <div className="pad">
              <Spinner />
            </div>
          )}
          {list.error && (
            <div className="pad">
              <ErrorBox errors={list.error} />
            </div>
          )}
          {list.data?.map((d) => (
            <a key={d.name} role="listitem" href={href(`design/${d.name}`)} className={"item" + (d.name === name ? " on" : "")}>
              <span className="item-name">
                {d.name}
                {drafts.has(d.name) && <span className="dot warn" title="Unsaved changes" />}
              </span>
              <span className="item-meta">{shortName(d.root?.table ?? "")}</span>
            </a>
          ))}
          {list.data?.length === 0 && <div className="muted small pad">No designs yet.</div>}
        </div>
      </aside>
      {name ? (
        <Editor key={name} name={name} onListChange={refreshList} />
      ) : (
        <section className="content">
          <Empty title={list.data?.length ? "Choose a design" : "Design your first document"}>
            <p>
              A design says which table's rows become documents, which related tables to pull in as fields or nested
              lists, and what each field is called. You can also start from a table in Explore.
            </p>
            <button className="btn primary" onClick={() => setShowNew(true)}>
              + New design
            </button>
          </Empty>
        </section>
      )}
      {showNew && (
        <NewDesignDialog
          existing={(list.data ?? []).map((d) => d.name)}
          onClose={() => setShowNew(false)}
          onCreated={(n) => {
            setShowNew(false);
            refreshList();
            go(`design/${n}`);
          }}
        />
      )}
    </div>
  );
}

function Editor({ name, onListChange }: { name: string; onListChange: () => void }) {
  const draft = drafts.get(name);
  const [design, setDesign] = useState<Design | null>(draft?.design ?? null);
  const [saved, setSaved] = useState(draft?.saved ?? "");
  const [loadError, setLoadError] = useState<string>();
  const [error, setError] = useState<string>();
  const [saving, setSaving] = useState(false);
  const [deploying, setDeploying] = useState(false);
  const [problems, setProblems] = useState<string[]>([]);

  useEffect(() => {
    if (drafts.get(name)) return;
    let live = true;
    api.design(name).then(
      (d) => {
        if (!live) return;
        const n = normalizeDesign(d);
        setDesign(n);
        setSaved(designKey(n));
      },
      (e) => live && setLoadError(message(e)),
    );
    return () => {
      live = false;
    };
  }, [name]);

  const dirty = design !== null && designKey(design) !== saved;
  useEffect(() => {
    if (design) drafts.set(name, dirty ? { design, saved } : null);
  }, [design, saved, dirty, name]);
  useEffect(() => {
    if (!dirty) return;
    const h = (e: BeforeUnloadEvent) => {
      e.preventDefault();
    };
    window.addEventListener("beforeunload", h);
    return () => window.removeEventListener("beforeunload", h);
  }, [dirty]);

  const save = useCallback(async () => {
    if (!design) return;
    setSaving(true);
    setError(undefined);
    try {
      await api.saveDesign(design);
      setSaved(designKey(design));
      onListChange();
    } catch (e) {
      setError(message(e));
    } finally {
      setSaving(false);
    }
  }, [design, onListChange]);

  const saveRef = useRef(save);
  saveRef.current = save;
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
        e.preventDefault();
        void saveRef.current();
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, []);

  async function rename(next: string) {
    if (!design || next === name) return;
    const exists = await api.design(next).then(
      () => true,
      () => false,
    );
    if (exists) throw new Error(`A design named ${next} already exists`);
    await api.saveDesign({ ...design, name: next });
    await api.deleteDesign(name);
    drafts.set(name, null);
    onListChange();
    go(`design/${next}`);
  }

  async function remove() {
    if (!window.confirm(`Delete the design "${name}"? A deployed pipeline keeps running until you stop it.`)) return;
    try {
      await api.deleteDesign(name);
      drafts.set(name, null);
      onListChange();
      go("design");
    } catch (e) {
      setError(message(e));
    }
  }

  if (loadError)
    return (
      <section className="content">
        <ErrorBox errors={loadError} title={`Cannot open design ${name}`} />
      </section>
    );
  if (!design)
    return (
      <section className="content pad">
        <Spinner />
      </section>
    );

  const update = (i: number, r: Relation) => setDesign({ ...design, related: design.related.map((x, j) => (j === i ? r : x)) });
  const removeRel = (i: number) => setDesign({ ...design, related: design.related.filter((_, j) => j !== i) });

  return (
    <div className="editor">
      <div className="editor-head">
        <NameField name={name} onRename={rename} />
        <span className={"chip" + (dirty ? " warn" : "")}>{dirty ? "Unsaved changes" : "Saved"}</span>
        <span className="spacer" />
        <button className="btn small ghost" onClick={remove}>
          Delete
        </button>
        <button className="btn" onClick={() => void save()} disabled={!dirty || saving} title="Save (⌘S / Ctrl+S)">
          {saving ? "Saving…" : "Save"}
        </button>
        <button className="btn primary" onClick={() => setDeploying(true)} disabled={!design.root.table}>
          Deploy…
        </button>
      </div>
      {error && (
        <div className="pad-s">
          <ErrorBox errors={error} />
        </div>
      )}
      <div className="editor-body">
        <div className="editor-main">
          <RootCard design={design} onChange={setDesign} errors={problems} />
          {design.root.table && (
            <>
              <div className="section-title">
                <h2>Related data</h2>
                <span className="muted small">
                  {design.related.length
                    ? `${design.related.length} ${design.related.length === 1 ? "table" : "tables"} pulled into each document`
                    : "Add data from other tables below"}
                </span>
              </div>
              {design.related.map((r, i) => (
                <RelationCard
                  key={r.id}
                  design={design}
                  index={i}
                  relation={r}
                  onChange={(r2) => update(i, r2)}
                  onRemove={() => removeRel(i)}
                />
              ))}
              <AddRelated design={design} onAdd={(r) => setDesign({ ...design, related: [...design.related, r] })} />
              <CalcCard design={design} onChange={setDesign} errors={problems} />
              <OutputCard design={design} onChange={setDesign} />
            </>
          )}
        </div>
        <Results design={design} onPreview={(r) => setProblems(r.errors)} />
      </div>
      {deploying && (
        <DeployDialog
          design={design}
          onClose={() => setDeploying(false)}
          onDeployed={() => {
            setSaved(designKey(design));
            drafts.set(name, null);
            onListChange();
            go("pipeline");
          }}
        />
      )}
    </div>
  );
}

function NameField({ name, onRename }: { name: string; onRename: (n: string) => Promise<void> }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(name);
  const [err, setErr] = useState<string>();
  const busy = useRef(false);

  if (!editing)
    return (
      <button
        className="design-name"
        onClick={() => {
          setValue(name);
          setErr(undefined);
          setEditing(true);
        }}
        title="Rename this design"
      >
        {name} <span className="faint">✎</span>
      </button>
    );

  async function commit() {
    if (busy.current) return;
    if (value === name) {
      setEditing(false);
      return;
    }
    if (!NAME_RE.test(value)) {
      setErr("Start with a lowercase letter; use a-z, 0-9 and _");
      return;
    }
    busy.current = true;
    try {
      await onRename(value);
      setEditing(false);
    } catch (e) {
      setErr(message(e));
    } finally {
      busy.current = false;
    }
  }
  return (
    <span className="name-edit">
      <input
        className="input mono"
        autoFocus
        value={value}
        aria-label="Design name"
        onChange={(e) => {
          setValue(e.target.value);
          setErr(undefined);
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter") void commit();
          if (e.key === "Escape") setEditing(false);
        }}
        onBlur={() => void commit()}
      />
      {err && <span className="error-text small">{err}</span>}
    </span>
  );
}

function OutputCard({ design, onChange }: { design: Design; onChange: (d: Design) => void }) {
  const o = design.output;
  const format = o.format === "ndjson" ? "ndjson" : "parquet_log";
  return (
    <section className="card">
      <header className="card-head">
        <div>
          <div className="eyebrow">Output</div>
          <h3>Where documents are written</h3>
        </div>
      </header>
      <div className="form-grid">
        <label>Format</label>
        <div>
          <Segmented
            value={format}
            options={[
              { value: "parquet_log", label: "Parquet segment log" },
              { value: "ndjson", label: "NDJSON files" },
            ]}
            onChange={(v) => onChange({ ...design, output: { format: v, include_json: v === "ndjson" ? false : o.include_json } })}
          />
        </div>
        {format === "parquet_log" && (
          <>
            <span />
            <label className="check">
              <input
                type="checkbox"
                checked={o.include_json}
                onChange={(e) => onChange({ ...design, output: { ...o, include_json: e.target.checked } })}
              />
              Also store each whole document as JSON (<span className="mono">_doc_json</span> column)
            </label>
          </>
        )}
      </div>
      <p className="hint">
        Deployed output goes to <span className="mono">pipelines/{design.name}/out</span> in the studio directory: a
        manifest, base snapshots and change segments, as described in docs/consumer-protocol.md.
      </p>
    </section>
  );
}

function NewDesignDialog({
  existing,
  onClose,
  onCreated,
}: {
  existing: string[];
  onClose: () => void;
  onCreated: (name: string) => void;
}) {
  const tables = useTables();
  const [root, setRoot] = useState("");
  const [name, setName] = useState("");
  const [touched, setTouched] = useState(false);
  const [err, setErr] = useState<string>();
  const [busy, setBusy] = useState(false);
  const used = new Set(existing);
  const nameErr =
    name && !NAME_RE.test(name)
      ? "Start with a lowercase letter; use a-z, 0-9 and _"
      : used.has(name)
        ? "A design with this name already exists"
        : undefined;

  function pick(t: string) {
    setRoot(t);
    if (!touched) setName(uniqueName(toId(shortName(t)), used));
  }
  async function create() {
    setBusy(true);
    setErr(undefined);
    try {
      const d = await loadTable(root);
      await api.saveDesign(newDesign(name, d.table));
      onCreated(name);
    } catch (e) {
      setErr(message(e));
      setBusy(false);
    }
  }
  return (
    <Modal
      title="New design"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!root || !name || !!nameErr || busy} onClick={create}>
            {busy ? "Creating…" : "Create design"}
          </button>
        </>
      }
    >
      <div className="form-stack">
        <label>
          Root table <span className="muted">· one document per row</span>
        </label>
        {tables.loading ? (
          <Spinner small />
        ) : (
          <Combobox value={root} options={rootOptions(tables.data)} onChange={pick} placeholder="Search tables" autoFocus ariaLabel="Root table" />
        )}
        <label>Design name</label>
        <input
          className={"input mono" + (nameErr ? " invalid" : "")}
          value={name}
          aria-label="Design name"
          onChange={(e) => {
            setName(e.target.value);
            setTouched(true);
          }}
        />
        {nameErr && <span className="error-text small">{nameErr}</span>}
        <p className="hint">The name becomes the document name and the folder of its deployed pipeline.</p>
      </div>
      {err && <ErrorBox errors={err} />}
    </Modal>
  );
}
