import { useEffect, useState } from "react";
import { api, message } from "../api";
import { href } from "../hooks";
import type { RuntimeState, RuntimeStatus } from "../types";
import { fmtTime } from "../util";
import { Empty, ErrorBox, Spinner } from "../components/ui";

const LABEL: Record<RuntimeState, string> = { stopped: "Stopped", starting: "Starting", running: "Running", error: "Error" };

export default function Pipeline() {
  const [st, setSt] = useState<RuntimeStatus | null>(null);
  const [err, setErr] = useState<string>();
  const [stopping, setStopping] = useState(false);

  useEffect(() => {
    let live = true;
    const tick = () =>
      api.runtime().then(
        (s) => {
          if (live) {
            setSt(s);
            setErr(undefined);
          }
        },
        (e) => live && setErr(message(e)),
      );
    void tick();
    const id = setInterval(tick, 2000);
    return () => {
      live = false;
      clearInterval(id);
    };
  }, []);

  async function stop() {
    if (!window.confirm("Stop the pipeline? It will not restart with the studio until you deploy again.")) return;
    setStopping(true);
    try {
      setSt(await api.stopRuntime());
    } catch (e) {
      setErr(message(e));
    } finally {
      setStopping(false);
    }
  }

  const state = st?.state ?? "stopped";
  const active = state === "running" || state === "starting";
  return (
    <div className="pipeline-page">
      <div className="pipeline-bar">
        <span className={"state " + state}>
          <span className="dot" />
          {st ? LABEL[state] : "…"}
        </span>
        {st?.design && (
          <span>
            design <a className="mono" href={href(`design/${st.design}`)}>{st.design}</a>
          </span>
        )}
        {fmtTime(st?.started_at) && <span className="muted">since {fmtTime(st?.started_at)}</span>}
        {st?.config_path && (
          <span className="muted small mono ellipsis" title={st.config_path}>
            {/* relative to the studio folder; the full path is on hover */}
            {/[\\/](pipelines[\\/].*)$/.exec(st.config_path)?.[1] ?? st.config_path}
          </span>
        )}
        <span className="spacer" />
        {state === "running" && (
          <a className="btn small" href="/pipeline/" target="_blank" rel="noreferrer">
            Open dashboard in a new tab
          </a>
        )}
        {active && (
          <button className="btn small danger" onClick={stop} disabled={stopping}>
            {stopping ? "Stopping…" : "Stop"}
          </button>
        )}
      </div>
      {err && (
        <div className="pad-s">
          <ErrorBox errors={err} />
        </div>
      )}
      {state === "error" && (
        <div className="pad-s">
          <ErrorBox errors={st?.error ?? "The pipeline stopped with an error."} title="The pipeline stopped" />
        </div>
      )}
      {state === "running" ? (
        <iframe className="dashboard-frame" src="/pipeline/" title="Pipeline dashboard" key={st?.started_at} />
      ) : state === "starting" ? (
        <Empty title="Starting the pipeline…">
          <Spinner />
        </Empty>
      ) : (
        <Empty title={state === "error" ? "Fix the problem and deploy again" : "No pipeline is running"}>
          <p>
            Open a design and choose <b>Deploy…</b>. Studio compiles it to a docsync config, starts the pipeline and shows its
            dashboard here: lag, throughput, pending work, outputs and a document inspector.
          </p>
          <a className="btn" href="#/design">
            Go to designs
          </a>
        </Empty>
      )}
    </div>
  );
}
