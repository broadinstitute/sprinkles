import { Link } from "react-router-dom";
import { useEffect, useState } from "react";
import { workerPoolColor } from "./JobsTable";
import { apiFetch } from "../api/client";

const C_OK = "oklch(45% 0.14 145)";
const C_BAD = "oklch(48% 0.20 25)";
const C_BAD_SOFT = "oklch(72% 0.16 25)";
const MONO = "'IBM Plex Mono', monospace";

export interface WorkpoolInfo {
  workpool_id: string;
  machine_type: string;
  region: string;
  state: string;
  state_message: string;
  last_incident_at: string | null;
  incident_count: number;
  expiry: string;
}

/** Polls a single workpool's info and active worker count. */
export function useWorkpoolSummary(
  workpoolId: string | undefined,
  active: boolean,
): { info: WorkpoolInfo | undefined; activeWorkerCount: number } {
  const [info, setInfo] = useState<WorkpoolInfo | undefined>();
  const [activeWorkerCount, setActiveWorkerCount] = useState(0);

  useEffect(() => {
    if (!workpoolId || !active) return;
    let cancelled = false;
    const poll = async () => {
      try {
        const r = await apiFetch(`/api/v1/workpool/${workpoolId}`);
        if (r.ok && !cancelled) setInfo(await r.json());
      } catch {
        /* transient */
      }
      try {
        const r = await apiFetch(
          `/api/v1/workpool/${workpoolId}/workers?status=started`,
        );
        if (r.ok && !cancelled) {
          const workers: unknown[] = await r.json();
          setActiveWorkerCount(workers.length);
        }
      } catch {
        /* transient */
      }
    };
    poll();
    const id = setInterval(poll, 10000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [workpoolId, active]);

  return { info, activeWorkerCount };
}

export default function WorkPoolSummaryCard({
  info: wp,
  activeWorkerCount,
  jobCount,
}: {
  info: WorkpoolInfo;
  activeWorkerCount: number;
  jobCount: number;
}) {
  const col = workerPoolColor(wp.workpool_id);
  const hasIncident = wp.incident_count > 0;

  return (
    <div
      style={{
        background: "white",
        border: "1.5px solid #ddd",
        borderRadius: 4,
        overflow: "hidden",
      }}
    >
      {/* Header */}
      <Link
        to={`/workpools/${wp.workpool_id}`}
        style={{ textDecoration: "none" }}
      >
        <div
          style={{
            padding: "8px 10px",
            background: "#fafafa",
            borderBottom: "1px solid #eee",
            display: "flex",
            alignItems: "center",
            gap: 6,
            cursor: "pointer",
          }}
        >
          <span
            style={{
              width: 9,
              height: 9,
              borderRadius: 2,
              flexShrink: 0,
              background: col.bg,
              border: `1.5px solid ${col.border}`,
            }}
          />
          <span
            style={{
              fontSize: 13,
              fontWeight: 700,
              fontFamily: MONO,
              color: "#222",
              overflow: "hidden",
              textOverflow: "ellipsis",
              whiteSpace: "nowrap",
              flex: 1,
            }}
          >
            {wp.workpool_id}
          </span>
          {wp.machine_type && (
            <span
              style={{
                fontSize: 11,
                fontFamily: MONO,
                color: "#888",
                flexShrink: 0,
              }}
            >
              {wp.machine_type}
            </span>
          )}
        </div>
      </Link>

      {/* Incident alert */}
      {hasIncident && (
        <div
          style={{
            padding: "7px 10px",
            background: "oklch(97% 0.04 25)",
            borderBottom: `1px solid ${C_BAD_SOFT}`,
            fontSize: 12,
            fontFamily: MONO,
            color: C_BAD,
            lineHeight: 1.45,
          }}
        >
          ⚠ {wp.incident_count} incident{wp.incident_count !== 1 ? "s" : ""}
          {wp.state_message && (
            <span style={{ color: "#555", fontWeight: 400 }}>
              {" — "}
              {wp.state_message}
            </span>
          )}
        </div>
      )}

      <div style={{ padding: "10px 12px 12px" }}>
        {/* Stats */}
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "1fr 1fr",
            gap: 8,
            marginBottom: 8,
          }}
        >
          {[
            {
              value: activeWorkerCount,
              label: "ACTIVE WORKERS",
              color: activeWorkerCount > 0 ? C_OK : "#bbb",
            },
            {
              value: jobCount,
              label: "JOBS",
              color: jobCount > 0 ? "#222" : "#bbb",
            },
          ].map(({ value, label, color }) => (
            <div
              key={label}
              style={{ display: "flex", flexDirection: "column" }}
            >
              <span
                style={{
                  fontSize: 18,
                  lineHeight: 1,
                  fontFamily: MONO,
                  fontWeight: 700,
                  color,
                }}
              >
                {value}
              </span>
              <span
                style={{
                  fontSize: 10,
                  letterSpacing: 1.2,
                  color: "#999",
                  fontFamily: MONO,
                  marginTop: 3,
                }}
              >
                {label}
              </span>
            </div>
          ))}
        </div>

        {wp.region && (
          <div
            style={{
              fontSize: 11,
              fontFamily: MONO,
              color: "#aaa",
              marginTop: 4,
            }}
          >
            {wp.region}
          </div>
        )}
      </div>
    </div>
  );
}
