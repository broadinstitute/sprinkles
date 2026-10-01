import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useEvents } from "../data/EventProvider";
import { RefreshToggle } from "../components/RefreshControls";
import { workerPoolColor } from "../components/JobsTable";
import type { WorkPoolListEntry } from "../types";
import { apiFetch } from "../api/client";

const POLL_INTERVAL_MS = 5_000;

const SYSTEM_LABEL_KEYS = new Set(["UUID"]);

function useWorkPools(active: boolean) {
  const [workpools, setWorkpools] = useState<WorkPoolListEntry[]>([]);
  const [workerCounts, setWorkerCounts] = useState<Record<string, number>>({});
  const [loaded, setLoaded] = useState(false);
  const [lastUpdatedAt, setLastUpdatedAt] = useState<number | null>(null);

  useEffect(() => {
    if (!active) return;
    let cancelled = false;
    async function poll() {
      while (!cancelled) {
        try {
          const res = await apiFetch("/api/v1/workpools");
          if (res.ok) {
            const data: WorkPoolListEntry[] = await res.json();
            const counts: Record<string, number> = {};
            await Promise.all(
              data.map(async (wp) => {
                try {
                  const r = await apiFetch(
                    `/api/v1/workpool/${wp.workpool_id}/workers?status=started`,
                  );
                  if (r.ok) {
                    const workers: unknown[] = await r.json();
                    counts[wp.workpool_id] = workers.length;
                  }
                } catch {
                  /* ignore */
                }
              }),
            );
            if (cancelled) return;
            setWorkpools(data);
            setWorkerCounts(counts);
            setLoaded(true);
            setLastUpdatedAt(Date.now());
          }
        } catch {
          // network errors are transient; just retry
        }
        await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
      }
    }
    poll();
    return () => {
      cancelled = true;
    };
  }, [active]);

  return { workpools, workerCounts, loaded, lastUpdatedAt };
}

function stateChipClass(state: string): string {
  if (state === "halted" || state === "unhealthy") return "wl-chip wl-chip-red";
  if (state === "ok" || state === "idle") return "wl-chip wl-chip-green";
  return "wl-chip";
}

export default function WorkPoolList() {
  const navigate = useNavigate();
  const { jobs, paused, setPaused } = useEvents();
  const { workpools, workerCounts, loaded, lastUpdatedAt } = useWorkPools(
    !paused,
  );
  const [search, setSearch] = useState("");

  const jobCounts = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const j of jobs)
      counts[j.workpool_id] = (counts[j.workpool_id] ?? 0) + 1;
    return counts;
  }, [jobs]);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    const sorted = [...workpools].sort((a, b) =>
      a.workpool_id.localeCompare(b.workpool_id),
    );
    if (!q) return sorted;
    return sorted.filter(
      (wp) =>
        wp.workpool_id.toLowerCase().includes(q) ||
        wp.state.toLowerCase().includes(q) ||
        wp.machine_type.toLowerCase().includes(q) ||
        wp.region.toLowerCase().includes(q) ||
        (wp.labels ?? []).some(
          (l) =>
            l.name.toLowerCase().includes(q) ||
            l.value.toLowerCase().includes(q) ||
            `${l.name}=${l.value}`.toLowerCase().includes(q),
        ),
    );
  }, [workpools, search]);

  return (
    <>
      <style>{styles}</style>
      <div className="wl-root">
        <div className="wl-filter-bar">
          <div className="wl-filter-search">
            <span className="wl-filter-label">Search</span>
            <input
              type="text"
              className="wl-search-input"
              placeholder="id, state, machine type, region, or label…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            {search && (
              <button className="wl-search-clear" onClick={() => setSearch("")}>
                ✕
              </button>
            )}
          </div>

          <div className="wl-filter-divider" />

          <RefreshToggle
            live={!paused}
            onToggle={() => setPaused(!paused)}
            lastUpdatedAt={lastUpdatedAt}
          />
        </div>

        <section className="wl-section">
          <h2 className="wl-section-title">Work Pools</h2>
          <p className="wl-subtitle">
            {filtered.length} work pool{filtered.length !== 1 ? "s" : ""} found
          </p>
          <div className="wl-divider" />
          {filtered.length === 0 ? (
            <div className="wl-empty">
              {loaded ? "no work pools found" : "loading…"}
            </div>
          ) : (
            <table className="wl-table">
              <thead>
                <tr>
                  <th className="wl-th wl-th-index" />
                  <th className="wl-th">Name</th>
                  <th className="wl-th wl-th-state">State</th>
                  <th className="wl-th wl-th-machine">Machine Type</th>
                  <th className="wl-th wl-th-region">Region</th>
                  <th className="wl-th wl-th-num">Active Workers</th>
                  <th className="wl-th wl-th-num">Jobs</th>
                  <th className="wl-th wl-th-num">Incidents</th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((wp, i) => {
                  const col = workerPoolColor(wp.workpool_id);
                  const workers = workerCounts[wp.workpool_id] ?? 0;
                  const jobCount = jobCounts[wp.workpool_id] ?? 0;
                  const labels = (wp.labels ?? []).filter(
                    (l) => !SYSTEM_LABEL_KEYS.has(l.name),
                  );
                  return (
                    <tr
                      key={wp.workpool_id}
                      className="wl-tr"
                      onClick={() => navigate(`/workpools/${wp.workpool_id}`)}
                    >
                      <td className="wl-td wl-td-index">
                        {String(i + 1).padStart(2, "0")}
                      </td>
                      <td className="wl-td">
                        <div className="wl-name">
                          <span
                            className="wl-swatch"
                            style={{
                              background: col.bg,
                              border: `1.5px solid ${col.border}`,
                            }}
                          />
                          <span className="wl-id">{wp.workpool_id}</span>
                        </div>
                        {wp.state_message && (
                          <div className="wl-message">{wp.state_message}</div>
                        )}
                        {labels.length > 0 && (
                          <div className="wl-labels">
                            {labels.map((l) => (
                              <span key={l.name} className="wl-label">
                                {l.name}={l.value}
                              </span>
                            ))}
                          </div>
                        )}
                      </td>
                      <td className="wl-td wl-td-state">
                        <span className={stateChipClass(wp.state)}>
                          {wp.state || "—"}
                        </span>
                      </td>
                      <td className="wl-td wl-td-small">
                        {wp.machine_type || "—"}
                      </td>
                      <td className="wl-td wl-td-small">{wp.region || "—"}</td>
                      <td
                        className={`wl-td wl-td-num${
                          workers > 0 ? " wl-num-ok" : ""
                        }`}
                      >
                        {workers}
                      </td>
                      <td className="wl-td wl-td-num">{jobCount}</td>
                      <td
                        className={`wl-td wl-td-num${
                          wp.incident_count > 0 ? " wl-num-bad" : ""
                        }`}
                      >
                        {wp.incident_count}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </section>

        <div className="wl-footer">◆ sprinkles dashboard</div>
      </div>
    </>
  );
}

const styles = `
  .wl-root {
    min-height: 100vh;
    background: #fff;
    color: #333;
    font-family: 'IBM Plex Mono', monospace;
    padding: 2rem;
    box-sizing: border-box;
  }

  /* ── Filter bar ─────────────────────────────────── */

  .wl-filter-bar {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 12px;
    border: 1px solid #e8e8e8;
    border-bottom: none;
    border-radius: 4px 4px 0 0;
    background: #fafafa;
  }

  .wl-filter-search {
    display: flex;
    align-items: center;
    gap: 6px;
    flex: 1;
    min-width: 0;
  }

  .wl-filter-label {
    font-size: 0.6rem;
    letter-spacing: 0.15em;
    text-transform: uppercase;
    color: #aaa;
    flex-shrink: 0;
  }

  .wl-search-input {
    font-family: 'IBM Plex Mono', monospace;
    font-size: 0.75rem;
    border: 1.5px solid #ccc;
    border-radius: 3px;
    padding: 4px 8px;
    outline: none;
    background: white;
    color: #111;
    flex: 1;
    min-width: 0;
    transition: border-color 0.12s;
  }

  .wl-search-input:focus { border-color: #333; }

  .wl-search-clear {
    font-family: 'IBM Plex Mono', monospace;
    font-size: 0.7rem;
    color: #aaa;
    background: none;
    border: none;
    cursor: pointer;
    padding: 0 2px;
    line-height: 1;
    flex-shrink: 0;
  }

  .wl-filter-divider {
    width: 1px;
    height: 20px;
    background: #e0e0e0;
    flex-shrink: 0;
  }

  /* ── Section ────────────────────────────────────── */

  .wl-section { margin-top: 1.5rem; margin-bottom: 3.5rem; }

  .wl-section-title {
    font-size: 0.65rem;
    font-weight: 700;
    letter-spacing: 0.2em;
    text-transform: uppercase;
    color: #888;
    margin: 0 0 0.25rem 0;
  }

  .wl-subtitle {
    font-size: 0.72rem;
    color: #bbb;
    margin: 0 0 0.75rem 0;
  }

  .wl-divider {
    height: 1px;
    background: linear-gradient(90deg, #1565c044, #1565c011 60%, transparent);
  }

  .wl-footer {
    margin-top: 2rem;
    font-size: 0.62rem;
    letter-spacing: 0.1em;
    color: #ddd;
    text-align: right;
  }

  /* ── Table ──────────────────────────────────────── */

  .wl-table {
    width: 100%;
    border-collapse: collapse;
    table-layout: fixed;
  }

  .wl-th {
    font-size: 0.6rem;
    letter-spacing: 0.18em;
    text-transform: uppercase;
    color: #aaa;
    font-weight: 400;
    padding: 0.45rem 0.5rem;
    text-align: left;
    border-bottom: 1px solid #f0f0f0;
  }

  .wl-th-index   { width: 2.2rem; }
  .wl-th-state   { width: 7rem; }
  .wl-th-machine { width: 11rem; }
  .wl-th-region  { width: 9rem; }
  .wl-th-num     { width: 8rem; text-align: right; }

  .wl-tr { cursor: pointer; }

  .wl-td {
    padding: 0.65rem 0.5rem;
    vertical-align: top;
    border-bottom: 1px solid #f0f0f0;
    transition: background 0.12s;
  }

  .wl-tr:hover .wl-td { background: #f0f5ff; }
  .wl-tr:hover .wl-id { color: #1565c0; }

  .wl-td-index {
    font-size: 0.65rem;
    color: #ccc;
    padding-top: 0.68rem;
    border-left: 2px solid transparent;
    transition: background 0.12s, border-color 0.15s;
  }

  .wl-tr:hover .wl-td-index { border-left-color: #1565c0; }

  .wl-name {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .wl-swatch {
    width: 9px;
    height: 9px;
    border-radius: 2px;
    flex-shrink: 0;
  }

  .wl-id {
    font-size: 0.82rem;
    font-weight: 500;
    color: #222;
    transition: color 0.12s;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .wl-message {
    font-size: 0.68rem;
    color: #b71c1c;
    margin-top: 0.25rem;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .wl-labels {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-top: 0.35rem;
  }

  .wl-label {
    font-size: 0.62rem;
    color: #777;
    background: #f5f5f5;
    border-radius: 3px;
    padding: 1px 6px;
  }

  .wl-td-state { padding-top: 0.68rem; }

  .wl-td-small {
    font-size: 0.72rem;
    color: #999;
    padding-top: 0.68rem;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .wl-td-num {
    text-align: right;
    font-size: 0.82rem;
    font-weight: 600;
    color: #bbb;
    padding-top: 0.6rem;
  }

  .wl-num-ok  { color: #2e7d32; }
  .wl-num-bad { color: #b71c1c; }

  .wl-chip {
    font-size: 0.68rem;
    font-weight: 600;
    border-radius: 4px;
    padding: 1px 8px;
    white-space: nowrap;
    background: #f0f0f0;
    color: #777;
  }

  .wl-chip-green { background: #e8f5e9; color: #2e7d32; }
  .wl-chip-red   { background: #ffebee; color: #b71c1c; }

  .wl-empty {
    font-size: 0.75rem;
    color: #ccc;
    padding: 1rem 0.5rem;
    border-bottom: 1px solid #f0f0f0;
  }
`;
