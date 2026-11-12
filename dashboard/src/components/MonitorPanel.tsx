import { useEffect, useState } from "react";
import { apiFetch } from "../api/client";
import type {
  MonitorFirestoreCall,
  MonitorPollerStatus,
  MonitorStatus,
} from "../types";

const POLL_INTERVAL_MS = 30_000;

// A poller that hasn't finished a run within this many of its max intervals
// is flagged as stale.
const STALE_FACTOR = 2;

type PollerHealth = "starting" | "ok" | "failing" | "stale";

function useMonitorStatus(): {
  status: MonitorStatus | undefined;
  /** When `status` was fetched (ms since epoch); relative times in the panel
   * are measured against this, so they only change on a refresh. */
  fetchedAt: number;
  /** When the next poll is due (ms since epoch); null while one is in flight. */
  nextRefreshAt: number | null;
} {
  const [status, setStatus] = useState<MonitorStatus>();
  const [fetchedAt, setFetchedAt] = useState(() => Date.now());
  const [nextRefreshAt, setNextRefreshAt] = useState<number | null>(null);
  useEffect(() => {
    let cancelled = false;
    async function poll() {
      while (!cancelled) {
        setNextRefreshAt(null);
        try {
          const res = await apiFetch("/api/v1/monitor/status");
          if (res.ok) {
            const data: MonitorStatus = await res.json();
            if (!cancelled) {
              setStatus(data);
              setFetchedAt(Date.now());
            }
          }
        } catch {
          // network errors are transient; just retry
        }
        setNextRefreshAt(Date.now() + POLL_INTERVAL_MS);
        await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
      }
    }
    poll();
    return () => {
      cancelled = true;
    };
  }, []);
  return { status, fetchedAt, nextRefreshAt };
}

/** Counts down to the next refresh. Ticks on its own so the rest of the panel
 * isn't re-rendered every second. */
function RefreshCountdown({ nextRefreshAt }: { nextRefreshAt: number | null }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  if (nextRefreshAt === null) return <>Refreshing…</>;
  const secs = Math.max(0, Math.round((nextRefreshAt - now) / 1000));
  return <>Will refresh in {secs} seconds…</>;
}

function pollerHealth(p: MonitorPollerStatus, now: number): PollerHealth {
  if (p.runs === 0 || !p.last_finished) return "starting";
  if (p.consecutive_failures > 0) return "failing";
  // A poller that is skipping runs because every workpool is idle is alive,
  // just idle, so count its last skip as activity.
  const lastActive = Math.max(
    new Date(p.last_finished).getTime(),
    p.last_skipped ? new Date(p.last_skipped).getTime() : 0,
  );
  const sinceLast = (now - lastActive) / 1000;
  if (sinceLast > p.max_interval_seconds * STALE_FACTOR) return "stale";
  return "ok";
}

function formatAgo(iso: string | null, now: number): string {
  if (!iso) return "never";
  const secs = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

function formatDuration(ms: number): string {
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`;
}

const HEALTH_LABEL: Record<PollerHealth, string> = {
  starting: "starting",
  ok: "ok",
  failing: "failing",
  stale: "stale",
};

const READ_OPS = new Set(["get", "query", "aggregate", "listen"]);
const WRITE_OPS = new Set(["write", "delete"]);

interface FirestorePollerRow {
  poller: string;
  calls: number;
  docsRead: number;
  docsWritten: number;
  /** Per (collection, op) detail, busiest first. */
  detail: MonitorFirestoreCall[];
}

/** Groups the per-(poller, collection, op) buckets into one row per poller,
 * busiest (most calls) first. */
function groupFirestoreCalls(
  calls: MonitorFirestoreCall[],
): FirestorePollerRow[] {
  const byPoller = new Map<string, FirestorePollerRow>();
  for (const c of calls) {
    let row = byPoller.get(c.poller);
    if (!row) {
      row = {
        poller: c.poller,
        calls: 0,
        docsRead: 0,
        docsWritten: 0,
        detail: [],
      };
      byPoller.set(c.poller, row);
    }
    row.calls += c.calls;
    if (READ_OPS.has(c.op)) row.docsRead += c.docs;
    if (WRITE_OPS.has(c.op)) row.docsWritten += c.docs;
    row.detail.push(c);
  }
  const rows = [...byPoller.values()];
  for (const r of rows) r.detail.sort((a, b) => b.calls - a.calls);
  return rows.sort((a, b) => b.calls - a.calls);
}

function FirestoreCallsSection({
  calls,
  since,
  now,
}: {
  calls: MonitorFirestoreCall[];
  since: string | null | undefined;
  now: number;
}) {
  const rows = groupFirestoreCalls(calls);
  const total = rows.reduce(
    (t, r) => ({
      calls: t.calls + r.calls,
      docsRead: t.docsRead + r.docsRead,
      docsWritten: t.docsWritten + r.docsWritten,
    }),
    { calls: 0, docsRead: 0, docsWritten: 0 },
  );
  const hours = since
    ? Math.max((now - new Date(since).getTime()) / 3_600_000, 1 / 60)
    : 0;
  const perHour = hours > 0 ? Math.round(total.calls / hours) : null;

  return (
    <div className="mon-fs">
      <div className="mon-header">
        <span className="mon-title">Firestore calls</span>
        <span className="mon-chip">
          {total.calls.toLocaleString()} calls
          {perHour !== null && ` · ~${perHour.toLocaleString()}/hour`}
        </span>
        <span className="mon-muted mon-fs-since">
          monitor only, since {since ? formatAgo(since, now) : "start"}
        </span>
      </div>
      {rows.length === 0 ? (
        <div className="mon-empty">No Firestore calls recorded yet.</div>
      ) : (
        <table className="mon-table">
          <thead>
            <tr>
              <th className="mon-th">Poller</th>
              <th className="mon-th mon-th-num">Calls</th>
              <th
                className="mon-th mon-th-num"
                title="Documents read: what Firestore bills for reads"
              >
                Docs read
              </th>
              <th className="mon-th mon-th-num">Docs written</th>
              <th className="mon-th">By collection &amp; op</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.poller} className="mon-tr">
                <td className="mon-td mon-name">
                  {r.poller || "(unattributed)"}
                </td>
                <td className="mon-td mon-num">{r.calls.toLocaleString()}</td>
                <td className="mon-td mon-num mon-muted">
                  {r.docsRead.toLocaleString()}
                </td>
                <td className="mon-td mon-num mon-muted">
                  {r.docsWritten.toLocaleString()}
                </td>
                <td className="mon-td">
                  {r.detail.map((d) => (
                    <span
                      key={`${d.collection}|${d.op}`}
                      className="mon-counter"
                      title={`${d.calls} calls, ${d.docs} documents`}
                    >
                      {d.collection || "—"} {d.op} <strong>{d.calls}</strong>
                      {d.docs > 0 && d.op !== "write" && d.op !== "delete" && (
                        <span className="mon-muted"> ({d.docs} docs)</span>
                      )}
                    </span>
                  ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export default function MonitorPanel() {
  const { status, fetchedAt: now, nextRefreshAt } = useMonitorStatus();

  const pollers = status?.pollers ?? [];
  const health = pollers.map((p) => pollerHealth(p, now));
  const failing = health.filter((h) => h === "failing").length;
  const stale = health.filter((h) => h === "stale").length;

  let summaryClass = "mon-chip mon-chip-green";
  let summary = "Healthy";
  if (!status) {
    summaryClass = "mon-chip";
    summary = "Loading…";
  } else if (!status.available) {
    summaryClass = "mon-chip";
    summary = "Unavailable";
  } else if (failing > 0 || stale > 0) {
    summaryClass = "mon-chip mon-chip-red";
    summary = [
      failing > 0 ? `${failing} failing` : "",
      stale > 0 ? `${stale} stale` : "",
    ]
      .filter(Boolean)
      .join(", ");
  } else if (health.every((h) => h === "starting")) {
    summaryClass = "mon-chip";
    summary = "Starting";
  }

  return (
    <>
      <style>{styles}</style>
      <section className="mon-panel">
        <div className="mon-header">
          <span className="mon-title">Monitor</span>
          <span className={summaryClass}>{summary}</span>
        </div>

        {status && !status.available ? (
          <div className="mon-empty">
            Monitor status is only available when the monitor runs in the same
            process as the dashboard (<code>sprinkles serve</code>).
          </div>
        ) : (
          <table className="mon-table">
            <thead>
              <tr>
                <th className="mon-th">Poller</th>
                <th className="mon-th mon-th-status">Status</th>
                <th className="mon-th mon-th-when">Last run</th>
                <th className="mon-th mon-th-num">Duration</th>
                <th className="mon-th mon-th-runs">Runs</th>
                <th
                  className="mon-th mon-th-num"
                  title="Runs started early by an event (a job or batch notification)"
                >
                  Wake-ups
                </th>
                <th className="mon-th">Work done</th>
              </tr>
            </thead>
            <tbody>
              {pollers.map((p, i) => (
                <tr key={p.name} className="mon-tr">
                  <td className="mon-td mon-name">{p.name}</td>
                  <td className="mon-td">
                    <span
                      className={`mon-status mon-status-${health[i]}`}
                      title={
                        p.last_error
                          ? `${p.last_error} (${formatAgo(
                              p.last_error_at,
                              now,
                            )})`
                          : undefined
                      }
                    >
                      {HEALTH_LABEL[health[i]]}
                      {p.consecutive_failures > 1
                        ? ` ×${p.consecutive_failures}`
                        : ""}
                    </span>
                  </td>
                  <td className="mon-td mon-muted">
                    {formatAgo(p.last_finished, now)}
                  </td>
                  <td className="mon-td mon-num mon-muted">
                    {p.runs > 0 ? formatDuration(p.last_duration_ms) : "—"}
                  </td>
                  <td className="mon-td mon-num mon-muted">
                    {p.runs}
                    {p.failures > 0 && (
                      <span className="mon-bad"> ({p.failures} failed)</span>
                    )}
                  </td>
                  <td className="mon-td mon-num mon-muted">
                    {p.notification_runs}
                  </td>
                  <td className="mon-td">
                    {Object.entries(p.counters)
                      .sort(([a], [b]) => a.localeCompare(b))
                      .map(([k, v]) => (
                        <span key={k} className="mon-counter">
                          {k.replace(/_/g, " ")} <strong>{v}</strong>
                        </span>
                      ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {status?.available && (
          <FirestoreCallsSection
            calls={status.firestore_calls ?? []}
            since={status.firestore_calls_since}
            now={now}
          />
        )}
        <div className="mon-refresh">
          <RefreshCountdown nextRefreshAt={nextRefreshAt} />
        </div>
      </section>
    </>
  );
}

const styles = `
  .mon-panel {
    border: 1px solid var(--bcl-border);
    border-radius: 8px;
    background: #fff;
    padding: 18px 20px;
    margin-bottom: 24px;
  }

  .mon-header {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-bottom: 12px;
  }

  .mon-title {
    font: 600 12px 'Open Sans', sans-serif;
    color: var(--bcl-gray);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }

  .mon-chip {
    font: 600 12px 'Open Sans', sans-serif;
    border-radius: 4px;
    padding: 2px 8px;
    background: var(--bcl-gray-bg);
    color: var(--bcl-gray);
  }

  .mon-chip-green { background: #F1F8EA; color: #4d7a1f; }
  .mon-chip-red   { background: #FBF0EE; color: var(--bcl-negative); }

  .mon-fs {
    margin-top: 20px;
    padding-top: 16px;
    border-top: 1px solid var(--bcl-border);
  }

  .mon-fs-since {
    font: 400 12px 'Open Sans', sans-serif;
  }

  .mon-refresh {
    margin-top: 10px;
    text-align: right;
    font: 400 11px 'Open Sans', sans-serif;
    color: var(--bcl-gray-light, #A0A2A5);
  }

  .mon-empty {
    font: 400 13px 'Open Sans', sans-serif;
    color: var(--bcl-gray);
    padding: 8px 0;
  }

  .mon-table {
    width: 100%;
    border-collapse: collapse;
    font: 400 12px 'Open Sans', sans-serif;
  }

  .mon-th {
    font: 600 11px 'Open Sans', sans-serif;
    color: var(--bcl-gray);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    text-align: left;
    padding: 6px 8px;
    border-bottom: 1px solid var(--bcl-border);
  }

  .mon-th-num,
  .mon-th-runs { text-align: right; }

  /* Every column but the last shrinks to fit its content (width: 1% plus
     nowrap makes an auto-layout column as narrow as it can be); the last
     column ("Work done") takes whatever width is left. */
  .mon-table th:not(:last-child),
  .mon-table td:not(:last-child) {
    width: 1%;
    white-space: nowrap;
    padding-right: 20px;
  }

  .mon-td {
    padding: 8px;
    vertical-align: top;
    border-bottom: 1px solid #f0f0f0;
  }

  .mon-tr:last-child .mon-td { border-bottom: none; }

  .mon-name { font: 500 12px 'IBM Plex Mono', monospace; color: var(--bcl-ink); }
  .mon-num { text-align: right; }
  .mon-muted { color: var(--bcl-gray); }
  .mon-bad { color: var(--bcl-negative); }

  .mon-status {
    font-weight: 600;
    border-radius: 4px;
    padding: 1px 8px;
    background: var(--bcl-gray-bg);
    color: var(--bcl-gray);
  }

  .mon-status-ok      { background: #F1F8EA; color: #4d7a1f; }
  .mon-status-failing { background: #FBF0EE; color: var(--bcl-negative); }
  .mon-status-stale   { background: #FFF4E0; color: #9a6200; }

  .mon-counter {
    display: inline-block;
    margin: 0 12px 2px 0;
    color: var(--bcl-gray);
  }

  .mon-counter strong { color: var(--bcl-ink); font-weight: 600; }
`;
