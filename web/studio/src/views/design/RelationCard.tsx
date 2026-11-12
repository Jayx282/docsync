import { useState } from "react";
import { api, message } from "../../api";
import { href, useTable } from "../../hooks";
import type { Design, JoinPair, Profile, Relation, Shape, Table } from "../../types";
import { eqi, IDENT_RE, isUnique, plural, shortName, uniqueName } from "../../util";
import { ErrorBox, Segmented, Spinner } from "../../components/ui";
import FieldPicker from "../../components/FieldPicker";
import { topLevelNames } from "./model";

export default function RelationCard({
  design,
  index,
  relation: r,
  onChange,
  onRemove,
}: {
  design: Design;
  index: number;
  relation: Relation;
  onChange: (r: Relation) => void;
  onRemove: () => void;
}) {
  const t = useTable(r.table);
  const [open, setOpen] = useState(true);
  const [profile, setProfile] = useState<Profile | null>(null);
  const [profiling, setProfiling] = useState(false);
  const [profileErr, setProfileErr] = useState<string>();
  const table = t.data?.table;
  const unique = table ? isUnique(table, r.join.map((j) => j.to)) : true;
  const set = (patch: Partial<Relation>) => onChange({ ...r, ...patch });

  function setShape(shape: Shape) {
    if (shape === r.shape) return;
    if (shape === "list") set({ shape, path: r.path || uniqueName(plural(r.id), topLevelNames(design, index)) });
    else set({ shape, path: "", sort_by: [] });
    setProfile(null);
  }

  async function measure() {
    setProfiling(true);
    setProfileErr(undefined);
    try {
      setProfile(await api.profile(design.root.table, r));
    } catch (e) {
      setProfileErr(message(e));
    } finally {
      setProfiling(false);
    }
  }

  const pathBad = !!r.path && !IDENT_RE.test(r.path);
  const summary = r.hidden ? (
    <>used only by calculated fields</>
  ) : r.shape === "list" ? (
      <>
        list <span className="mono">{r.path || "(unnamed)"}</span> · {r.fields.length} fields per item
      </>
    ) : r.path ? (
      <>
        object <span className="mono">{r.path}</span> · {r.fields.length} fields
      </>
    ) : (
      <>{r.fields.length} top-level fields</>
    );

  return (
    <section className={"card relation" + (open ? " open" : "")}>
      <header className="card-head clickable" onClick={() => setOpen(!open)}>
        <div className="rel-title">
          <span className={"shape-icon " + r.shape} aria-hidden="true">
            {r.shape === "list" ? "[ ]" : "{ }"}
          </span>
          <div>
            <h3>
              {shortName(r.table)} {r.via && <span className="muted">via {shortName(r.via.table)}</span>}
            </h3>
            <div className="muted small">{summary}</div>
          </div>
        </div>
        <div className="actions" onClick={(e) => e.stopPropagation()}>
          <button className="btn small ghost" onClick={onRemove} aria-label={`Remove ${r.table}`}>
            Remove
          </button>
          <button className="icon-btn" onClick={() => setOpen(!open)} aria-expanded={open} aria-label={open ? "Collapse" : "Expand"}>
            {open ? "▾" : "▸"}
          </button>
        </div>
      </header>
      {open && (
        <div className="card-body">
          <JoinLine design={design} relation={r} />
          <label className="check small hidden-toggle">
            <input type="checkbox" checked={!!r.hidden} onChange={(e) => set({ hidden: e.target.checked || undefined })} />
            Use only in calculated fields (leave its rows out of documents)
          </label>
          {r.hidden && (
            <p className="hint">
              Calculated fields can use it, for example{" "}
              <span className="mono">{r.shape === "list" || r.via ? `COUNT(${r.id})` : `${r.id}.${r.join[0]?.to ?? "column"}`}</span>. Its
              changes still rebuild the documents that use it.
            </p>
          )}
          <div className="form-grid">
            <label>Show as</label>
            <div>
              <Segmented
                value={r.shape}
                options={[
                  {
                    value: "fields",
                    label: "Fields (one row)",
                    disabled: r.via ? "Rows reached through a bridge table can be many; show them as a list" : undefined,
                  },
                  { value: "list", label: "List (many rows)" },
                ]}
                onChange={setShape}
              />
              {r.shape === "fields" && !unique && table && (
                <p className="warn-text small">
                  {shortName(r.table)}.{r.join.map((j) => j.to).join(", ")} is not unique, so several rows can match one{" "}
                  {shortName(design.root.table)}. Documents with more than one match fail to build. Show it as a list, or
                  join on a unique column.
                </p>
              )}
            </div>
            {!r.hidden && <label htmlFor={`path-${r.id}`}>{r.shape === "list" ? "List name" : "Nest under"}</label>}
            {!r.hidden && <div>
              <input
                id={`path-${r.id}`}
                className={"input mono" + (pathBad || (r.shape === "list" && !r.path) ? " invalid" : "")}
                value={r.path ?? ""}
                placeholder={r.shape === "list" ? "required" : "optional · empty puts fields at the top level"}
                onChange={(e) => set({ path: e.target.value.trim() })}
              />
            </div>}
            {r.shape === "list" && table && !r.hidden && (
              <>
                <label>Sort items by</label>
                <SortPicker
                  table={table}
                  relation={r}
                  onChange={(sort_by) => {
                    // Sort columns must be part of each item; add them if needed.
                    const fields = [...r.fields];
                    for (const s of sort_by)
                      if (!fields.some((f) => eqi(f.column, s)) && !table.primary_key.some((k) => eqi(k, s))) fields.push({ column: s });
                    set({ sort_by, fields });
                  }}
                />
              </>
            )}
          </div>
          {t.loading && <Spinner />}
          {t.error && <ErrorBox errors={t.error} />}
          {table && !r.hidden && (
            <FieldPicker
              columns={table.columns}
              fields={r.fields}
              pk={table.primary_key}
              joined={r.join.map((j) => j.to)}
              onChange={(fields) => set({ fields })}
            />
          )}
          <div className="measure">
            <button className="btn small" onClick={measure} disabled={profiling}>
              {profiling && <Spinner small />} Measure on live data
            </button>
            {profile && <ProfileView profile={profile} relation={r} />}
            {profileErr && <span className="error-text small">{profileErr}</span>}
          </div>
        </div>
      )}
    </section>
  );
}

function JoinLine({ design, relation: r }: { design: Design; relation: Relation }) {
  const root = shortName(design.root.table);
  const rel = shortName(r.table);
  const pairs = (a: string, b: string, j: JoinPair[]) =>
    j.map((p, i) => (
      <span key={i} className="join-pair mono">
        {a}.{p.from} = {b}.{p.to}
      </span>
    ));
  return (
    <div className="join-line">
      <span className="muted">Joined on</span>
      {r.via ? (
        <>
          {pairs(root, shortName(r.via.table), r.via.join)}
          <span className="muted">then</span>
          {pairs(shortName(r.via.table), rel, r.join)}
        </>
      ) : (
        pairs(root, rel, r.join)
      )}
      <a className="small" href={href(`explore/${r.table}`)}>
        open table
      </a>
    </div>
  );
}

function SortPicker({ table, relation: r, onChange }: { table: Table; relation: Relation; onChange: (s: string[]) => void }) {
  const sort = r.sort_by ?? [];
  const avail = table.columns.filter((c) => !sort.some((s) => eqi(s, c.name)));
  return (
    <div className="sort-picker">
      {sort.length === 0 && <span className="muted small">primary key ({table.primary_key.join(", ") || "none"})</span>}
      {sort.map((s) => (
        <span key={s} className="chip removable mono">
          {s}
          <button onClick={() => onChange(sort.filter((x) => x !== s))} aria-label={`Stop sorting by ${s}`}>
            ×
          </button>
        </span>
      ))}
      <select
        className="input small"
        value=""
        aria-label="Add sort column"
        onChange={(e) => e.target.value && onChange([...sort, e.target.value])}
      >
        <option value="">+ column</option>
        {avail.map((c) => (
          <option key={c.name} value={c.name}>
            {c.name}
          </option>
        ))}
      </select>
    </div>
  );
}

function ProfileView({ profile: p, relation: r }: { profile: Profile; relation: Relation }) {
  return (
    <div className="profile">
      <p className="small">{p.explanation}</p>
      <div className="profile-stats">
        {r.shape === "list" ? (
          <>
            <span>
              <b>{p.max_per_document}</b> max items
            </span>
            <span>
              <b>{p.avg_per_document.toFixed(1)}</b> avg items
            </span>
            <span>
              <b>{p.documents_with_any}</b> documents with items
            </span>
          </>
        ) : (
          <>
            <span>
              <b>{p.fanout_max}</b> documents share one row (max)
            </span>
            <span>
              <b>{p.fanout_avg.toFixed(1)}</b> avg
            </span>
          </>
        )}
        <span className="faint">{p.took_ms} ms</span>
      </div>
    </div>
  );
}
