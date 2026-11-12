import { useEffect, useMemo, useState } from "react";
import { api, message } from "../../api";
import { useAsync } from "../../hooks";
import type { Design, DeployPlan, ValidateResult } from "../../types";
import { fmtTime, shortName } from "../../util";
import { Code, ErrorBox, Modal, Spinner } from "../../components/ui";
import { diffLines, withContext } from "../../diff";
import { FindingList } from "./Results";

export default function DeployDialog({
  design,
  onClose,
  onDeployed,
}: {
  design: Design;
  onClose: () => void;
  onDeployed: () => void;
}) {
  const [v, setV] = useState<ValidateResult | null>(null);
  const [err, setErr] = useState<string>();
  const [busy, setBusy] = useState(false);
  const plan = useAsync(() => api.deployPlan(design), []);

  useEffect(() => {
    let live = true;
    api.validate(design).then(
      (r) => live && setV(r),
      (e) => live && setErr(message(e)),
    );
    return () => {
      live = false;
    };
    // Checks run once, for the design as it was when the dialog opened.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const fails = v?.findings.filter((f) => f.severity === "FAIL") ?? [];
  const warns = v?.findings.filter((f) => f.severity === "warn") ?? [];
  const blocked = !v || v.errors.length > 0 || !plan.data;
  const p = plan.data;
  const running = p && (p.running.state === "running" || p.running.state === "starting") ? p.running.design : undefined;

  async function deploy() {
    setBusy(true);
    setErr(undefined);
    try {
      await api.deploy(design);
      onDeployed();
    } catch (e) {
      setErr(message(e));
      setBusy(false);
    }
  }

  return (
    <Modal
      title={`Deploy ${design.name}`}
      onClose={onClose}
      wide
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className={"btn " + (fails.length ? "danger" : "primary")} disabled={blocked || busy} onClick={deploy}>
            {busy ? "Deploying…" : fails.length ? "Deploy anyway" : "Save and deploy"}
          </button>
        </>
      }
    >
      <h4 className="first">What changes</h4>
      {plan.loading && (
        <p className="row">
          <Spinner small /> Comparing with the deployed version…
        </p>
      )}
      {plan.error && <ErrorBox errors={plan.error} />}
      {p && <ChangeSummary plan={p} design={design} running={running} />}

      <h4>Checks</h4>
      {!v && !err && (
        <p className="row">
          <Spinner small /> Running checks…
        </p>
      )}
      {v && v.errors.length > 0 && <ErrorBox errors={v.errors} title="Fix the design first" />}
      {v && v.errors.length === 0 && (
        <>
          <FindingList findings={v.findings} only={["FAIL", "warn"]} />
          {fails.length === 0 && warns.length === 0 && <p className="ok-text">All checks passed.</p>}
          {fails.length > 0 && (
            <>
              <p>The pipeline will likely stop with an error until the failed checks are fixed. Your DBA can run:</p>
              <Code text={v.script} lang="sql" copy downloadAs="docsync-setup.sql" />
            </>
          )}
        </>
      )}
      {err && <ErrorBox errors={err} />}
    </Modal>
  );
}

function ChangeSummary({ plan, design, running }: { plan: DeployPlan; design: Design; running?: string }) {
  const [open, setOpen] = useState(false);
  const lines = useMemo(() => diffLines(plan.current, plan.next), [plan.current, plan.next]);
  const added = lines.filter((l) => l.kind === "add").length;
  const removed = lines.filter((l) => l.kind === "del").length;
  const root = <span className="mono">{shortName(design.root.table)}</span>;

  if (plan.errors.length > 0) return <ErrorBox errors={plan.errors} title="The design cannot be compiled" />;
  return (
    <div className="change-summary">
      {!plan.deployed && (
        <p>
          First deployment: the pipeline loads every {root} row{design.root.filter ? " that matches the filter" : ""}, then
          rebuilds only documents whose data changed.
        </p>
      )}
      {plan.deployed && plan.same && (
        <p>
          Nothing changed since the last deployment ({fmtTime(plan.deployed_at)}). Deploying restarts the pipeline, which
          continues where it stopped.
        </p>
      )}
      {plan.deployed && !plan.same && plan.rebuild && (
        <div className="callout warn">
          <b>Every document is rebuilt.</b> The document definition changed, so the pipeline starts a new epoch: it rebuilds
          all documents and writes a new base snapshot. Consumers that follow the manifest switch to the new base on their
          own.
        </div>
      )}
      {plan.deployed && !plan.same && !plan.rebuild && (
        <p>Only settings change. Documents stay as they are, and the pipeline continues where it stopped.</p>
      )}
      {running && running !== design.name && (
        <p className="warn-text">
          The running pipeline <span className="mono">{running}</span> stops first. Only one pipeline runs at a time.
        </p>
      )}
      {plan.deployed && !plan.same && (
        <>
          <button className="link" onClick={() => setOpen(!open)} aria-expanded={open}>
            {open ? "Hide" : "Show"} config changes{" "}
            <span className="diff-count">
              <span className="add">+{added}</span> <span className="del">−{removed}</span>
            </span>
          </button>
          {open && (
            <div className="diff" role="region" aria-label="Config changes">
              {withContext(lines).map((row, i) =>
                row.kind === "gap" ? (
                  <div key={i} className="diff-gap">
                    ⋯ {row.count} unchanged {row.count === 1 ? "line" : "lines"}
                  </div>
                ) : (
                  <div key={i} className={"diff-line " + row.kind}>
                    <span className="diff-sign">{row.kind === "add" ? "+" : row.kind === "del" ? "−" : " "}</span>
                    {row.text || " "}
                  </div>
                ),
              )}
            </div>
          )}
        </>
      )}
    </div>
  );
}
