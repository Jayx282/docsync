"use strict";

const $ = (id) => document.getElementById(id);

function ago(iso) {
  if (!iso || iso.startsWith("0001-")) return "never";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 1) return "just now";
  if (s < 60) return `${s.toFixed(0)}s ago`;
  if (s < 3600) return `${(s / 60).toFixed(0)}m ago`;
  if (s < 86400) return `${(s / 3600).toFixed(1)}h ago`;
  return `${(s / 86400).toFixed(1)}d ago`;
}

function num(n) { return (n ?? 0).toLocaleString(); }

function cell(text, cls) {
  const td = document.createElement("td");
  td.textContent = text;
  if (cls) td.className = cls;
  return td;
}

function row(...cells) {
  const tr = document.createElement("tr");
  cells.forEach((c) => tr.appendChild(c));
  return tr;
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

function render(s) {
  $("doc-name").textContent = s.document ? `· ${s.document}` : "";
  const ck = s.checkpoint || {};
  const pill = $("state-pill");
  let state = "running", cls = "ok";
  if (s.paused) { state = "paused"; cls = "bad"; }
  else if (s.activity) { state = s.activity.split(":")[0]; cls = "warn"; }
  else if (ck.snapshotting) { state = "snapshotting"; cls = "warn"; }
  else if (s.pending > 0) { state = "catching up"; cls = "warn"; }
  pill.textContent = state;
  pill.className = `pill ${cls}`;

  const banner = $("banner");
  if (s.paused) {
    banner.textContent = `Paused: ${s.pause_reason || "by operator"}`;
    banner.className = "banner bad";
  } else if (s.activity) {
    banner.textContent = s.activity;
    banner.className = "banner";
  } else {
    banner.className = "banner hidden";
  }

  $("c-last-cycle").textContent = ago(s.last_cycle_at);
  $("c-cycle-time").textContent = s.last_cycle_duration_ns ? `took ${(s.last_cycle_duration_ns / 1e6).toFixed(1)} ms` : "";
  $("c-last-change").textContent = ago(s.last_change_at);
  $("c-ct-version").textContent = `change tracking version ${num(ck.ct_version)}`;
  $("c-generation").textContent = num(ck.generation);
  $("c-epoch").textContent = `epoch ${ck.epoch ?? "–"}`;
  $("c-pending").textContent = num(s.pending);
  $("c-upserts").textContent = num(s.upserts);
  $("c-deletes").textContent = `${num(s.deletes)} deletes · ${num(s.roots_assembled)} assembled`;
  $("c-root-errors").textContent = num(s.root_errors);
  $("c-cycles").textContent = `${num(s.cycles)} cycles · ${num(s.changes)} source changes`;
  $("c-sweep").textContent = num(s.sweep_corrections);

  const sinks = $("sinks");
  sinks.replaceChildren(...(s.sinks || []).map((k) => {
    const base = k.base_in_flight ? "writing…" : (k.has_base ? ago(k.base_at) : "none yet");
    const lag = cell(k.lag_generations ? `${num(k.lag_generations)} gen` : "up to date", "num");
    return row(cell(k.name), cell(num(k.generation), "num"), lag, cell(base));
  }));

  const tables = $("tables");
  const entries = Object.entries(s.tables || {}).sort((a, b) => a[0].localeCompare(b[0]));
  tables.replaceChildren(...entries.map(([name, t]) =>
    row(cell(name), cell(num(t.changes), "num"), cell(ago(t.last_change_at)), cell(`${num(t.last_roots)} / ${num(t.max_roots)}`, "num"))));
  if (entries.length === 0) tables.replaceChildren(row(cell("no changes captured yet")));

  $("last-error").textContent = s.last_error ? `${ago(s.last_error_at)}: ${s.last_error}` : "none";
}

async function refresh() {
  try { render(await api("api/status")); }
  catch (e) { $("state-pill").textContent = "unreachable"; $("state-pill").className = "pill bad"; }
}

async function act(action, body) {
  $("action-result").textContent = `${action}…`;
  try {
    await api(`api/actions/${action}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
    $("action-result").textContent = `${action}: done`;
  } catch (e) {
    $("action-result").textContent = `${action} failed: ${e.message}`;
  }
  refresh();
}

document.querySelectorAll("button[data-action]").forEach((b) => {
  b.addEventListener("click", () => {
    if (b.dataset.confirm && !confirm(b.dataset.confirm)) return;
    act(b.dataset.action);
  });
});

$("rebuild-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const ids = $("rebuild-ids").value.split(",").map((x) => x.trim()).filter(Boolean);
  if (ids.length) act("rebuild", { ids });
});

$("rebuild-all").addEventListener("click", () => {
  if (confirm("Rebuild every document? Unchanged documents are not re-emitted.")) act("rebuild", { all: true });
});

function pretty(json, diffs) {
  const pre = document.createDocumentFragment();
  if (!json) { pre.appendChild(document.createTextNode("(none)")); return pre; }
  const obj = JSON.parse(json);
  for (const key of Object.keys(obj).sort()) {
    const span = document.createElement("span");
    span.textContent = `"${key}": ${JSON.stringify(obj[key], null, 2)}\n`;
    if (diffs.includes(key)) span.className = "diff";
    pre.appendChild(span);
  }
  return pre;
}

$("inspect-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const id = $("inspect-id").value.trim();
  const out = $("inspect-result");
  out.classList.remove("hidden");
  $("inspect-summary").textContent = "loading…";
  try {
    const r = await api(`api/docs/${encodeURIComponent(id)}`);
    const parts = [];
    parts.push(r.exists ? "exists in the database" : "does not exist in the database");
    parts.push(r.stored ? `last emitted at generation ${r.stored_generation}` : "never emitted");
    if (r.pending) parts.push("queued for rebuild");
    if (r.diff_fields.length) parts.push(`differs in: ${r.diff_fields.join(", ")} (a rebuild is pending or a change has not been captured yet)`);
    else if (r.stored && r.live) parts.push("in sync");
    if (r.error) parts.push(`assembly error: ${r.error}`);
    if (r.last_error) parts.push(`last recorded error: ${r.last_error}`);
    $("inspect-summary").textContent = `Root ${r.root}: ${parts.join(" · ")}`;
    $("inspect-stored").replaceChildren(pretty(r.stored, r.diff_fields));
    $("inspect-live").replaceChildren(pretty(r.live, r.diff_fields));
  } catch (e) {
    $("inspect-summary").textContent = `Error: ${e.message}`;
    $("inspect-stored").textContent = "";
    $("inspect-live").textContent = "";
  }
});

refresh();
setInterval(refresh, 2000);
