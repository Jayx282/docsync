import { useEffect, useState } from "react";
import { api, message, onUnauthorized } from "./api";
import { href, schemaChanged, useRoute } from "./hooks";
import type { Connection } from "./types";
import { Logo, Spinner } from "./components/ui";
import Explore from "./views/Explore";
import DesignView from "./views/Design";
import Pipeline from "./views/Pipeline";

export default function App() {
  const route = useRoute();
  const view = route[0] === "design" || route[0] === "pipeline" ? route[0] : "explore";
  const [conn, setConn] = useState<Connection | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [lastDesign, setLastDesign] = useState<string>();
  const [lastTable, setLastTable] = useState<string>();

  useEffect(() => onUnauthorized(() => setUnauthorized(true)), []);
  useEffect(() => {
    api.connection().then(setConn, (e) => setConn({ ok: false, error: message(e) }));
  }, []);
  useEffect(() => {
    if (view === "design" && route[1]) setLastDesign(route[1]);
    if (view === "explore" && route[1]) setLastTable(route[1]);
  }, [view, route]);

  async function refresh() {
    setRefreshing(true);
    try {
      await api.refreshSchema();
      schemaChanged();
      setConn(await api.connection());
    } catch (e) {
      setConn({ ok: false, error: message(e) });
    } finally {
      setRefreshing(false);
    }
  }

  if (unauthorized) return <SignedOut />;

  const tabs = [
    { id: "explore", label: "Explore", to: lastTable ? href(`explore/${lastTable}`) : "#/explore" },
    { id: "design", label: "Design", to: lastDesign ? href(`design/${lastDesign}`) : "#/design" },
    { id: "pipeline", label: "Pipeline", to: "#/pipeline" },
  ];
  return (
    <div className="app">
      <header className="topbar">
        <a className="brand" href="#/explore">
          <Logo />
          <span>docsync</span>
          <span className="brand-sub">studio</span>
        </a>
        <nav className="tabs" aria-label="Sections">
          {tabs.map((t) => (
            <a key={t.id} href={t.to} className={view === t.id ? "tab on" : "tab"} aria-current={view === t.id ? "page" : undefined}>
              {t.label}
            </a>
          ))}
        </nav>
        <span className="spacer" />
        <ConnectionPill conn={conn} />
        <button
          className="btn small ghost"
          onClick={refresh}
          disabled={refreshing}
          title="Reload tables, columns, keys and indexes from the database"
        >
          {refreshing ? <Spinner small /> : <span aria-hidden="true">↻</span>} Refresh schema
        </button>
      </header>
      <main className="main">
        {view === "design" ? (
          <DesignView name={route[1]} />
        ) : view === "pipeline" ? (
          <Pipeline />
        ) : (
          <Explore table={route[1]} conn={conn} />
        )}
      </main>
    </div>
  );
}

function ConnectionPill({ conn }: { conn: Connection | null }) {
  if (!conn)
    return (
      <span className="conn">
        <Spinner small /> Connecting…
      </span>
    );
  if (!conn.ok)
    return (
      <span className="conn bad" title={conn.error}>
        <span className="dot bad" /> Not connected
      </span>
    );
  return (
    <span className="conn" title={`${conn.edition ?? ""} ${conn.version ?? ""}`.trim()}>
      <span className="dot ok" />
      <b>{conn.database}</b>
      <span className="muted">on {conn.server}</span>
      {!conn.change_tracking && (
        <span className="chip warn" title="Enable change tracking on the database before deploying (see Checks)">
          change tracking off
        </span>
      )}
      {!conn.snapshot_isolation && (
        <span className="chip warn" title="docsync reads in snapshot transactions (see Checks)">
          snapshot isolation off
        </span>
      )}
    </span>
  );
}

function SignedOut() {
  return (
    <div className="signed-out">
      <div className="card">
        <Logo />
        <h2>Sign in to docsync studio</h2>
        <p>This browser is no longer signed in. Its token may have been replaced, for example after the studio's token file was deleted.</p>
        <p>
          Sign in again with the studio's access token. You'll come back to this page.
        </p>
        {/* Reloading lets the server show its sign-in form, which keeps the current #/route. */}
        <button className="btn primary" onClick={() => window.location.reload()}>
          Sign in
        </button>
      </div>
    </div>
  );
}
