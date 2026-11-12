import { useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";
import { apiFetch } from "../api/client";
import { RefreshToggle } from "../components/RefreshControls";
import type { ErrorLogEntry } from "../types";
import { useUnseenErrors } from "../data/UnseenErrors";
import ErrorLogList from "../components/ErrorLogList";
import MonitorPanel from "../components/MonitorPanel";
import TabBar from "../components/TabBar";

const MONO = "'IBM Plex Mono', monospace";
const POLL_INTERVAL_MS = 30_000;

// Saves the current errors as a plain-text file, one "[timestamp] message"
// entry per line (multi-line messages keep their own line breaks).
function downloadLog(errors: ErrorLogEntry[]) {
  const text =
    errors.map((e) => `[${e.timestamp}] ${e.message}`).join("\n") + "\n";
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = `sprinkles-error-log-${new Date()
    .toISOString()
    .replace(/[:.]/g, "-")}.txt`;
  a.click();
  URL.revokeObjectURL(url);
}

// The error log tab. It polls only while it's showing, which is also what
// makes viewing it count as having seen the errors (see markSeen).
function ErrorLogTab() {
  const [errors, setErrors] = useState<ErrorLogEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [paused, setPaused] = useState(false);
  const [lastUpdatedAt, setLastUpdatedAt] = useState<number | null>(null);

  const { markSeen } = useUnseenErrors();

  const bottomRef = useRef<HTMLDivElement>(null);
  const lastKeyRef = useRef<string | null>(null);
  const hasLoadedRef = useRef(false);

  // Poll GET /api/v1/errors every 30s. The endpoint returns the full current
  // list each time (no cursor), so each successful poll just replaces state.
  useEffect(() => {
    if (paused) return;
    let cancelled = false;

    async function poll() {
      while (!cancelled) {
        try {
          const res = await apiFetch("/api/v1/errors");
          if (res.ok && !cancelled) {
            const data: { errors: ErrorLogEntry[] } = await res.json();
            setErrors(data.errors ?? []);
            const newest = data.errors?.[data.errors.length - 1];
            if (newest) markSeen(newest.timestamp);
            setLastUpdatedAt(Date.now());
            setLoading(false);
          }
        } catch {
          // network errors are transient; just retry next tick
        }
        await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
      }
    }
    poll();
    return () => {
      cancelled = true;
    };
  }, [paused, markSeen]);

  // Auto-scroll to the bottom whenever a new error has arrived since the
  // last render: jump instantly into view on first load, then smooth-scroll
  // on later arrivals so a fresh error is never left off-screen.
  useEffect(() => {
    if (errors.length === 0) return;
    const last = errors[errors.length - 1];
    const key = `${last.timestamp}|${last.message}`;
    if (key === lastKeyRef.current) return;
    const isFirstLoad = !hasLoadedRef.current;
    lastKeyRef.current = key;
    hasLoadedRef.current = true;
    bottomRef.current?.scrollIntoView({
      behavior: isFirstLoad ? "auto" : "smooth",
    });
  }, [errors]);

  return (
    <>
      <div
        style={{
          display: "flex",
          alignItems: "flex-start",
          justifyContent: "space-between",
          gap: "1rem",
          marginBottom: "1rem",
        }}
      >
        <p
          style={{
            color: "#888",
            fontSize: "0.85rem",
            margin: 0,
          }}
        >
          Recent errors from the monitor's background pollers (cluster
          reconciler, provisioning, task recovery, batch startup monitor,
          summary polls, etc.), polled every 30s.
        </p>
        <div
          style={{
            display: "flex",
            alignItems: "center",
            gap: "1rem",
            flexShrink: 0,
          }}
        >
          <button
            type="button"
            onClick={() => downloadLog(errors)}
            disabled={errors.length === 0}
            style={{
              fontFamily: MONO,
              fontSize: "0.8rem",
              padding: "4px 10px",
              border: "1px solid #ccc",
              borderRadius: 4,
              background: "white",
              cursor: errors.length === 0 ? "not-allowed" : "pointer",
              opacity: errors.length === 0 ? 0.5 : 1,
            }}
          >
            Download log
          </button>
          <RefreshToggle
            live={!paused}
            onToggle={() => setPaused((p) => !p)}
            lastUpdatedAt={lastUpdatedAt}
          />
        </div>
      </div>

      <ErrorLogList errors={errors} loading={loading} bottomRef={bottomRef} />
    </>
  );
}

export default function MonitorDashboard() {
  const location = useLocation();
  const { hasUnseen } = useUnseenErrors();
  const isStatsTab = location.pathname.endsWith("/monitor/stats");

  const tabs = [
    {
      // Flag new errors from the other tab; on this tab they're being shown.
      label: hasUnseen && isStatsTab ? "Error log (new)" : "Error log",
      href: "/monitor",
      matchExact: true,
    },
    { label: "Stats", href: "/monitor/stats", matchExact: true },
  ];

  return (
    <div
      style={{
        padding: "2rem",
        fontFamily: MONO,
        maxWidth: 1100,
        margin: "0 auto",
      }}
    >
      <h1
        style={{
          fontSize: "1.6rem",
          fontWeight: 700,
          color: "#111",
          margin: "0 0 1.5rem",
        }}
      >
        Monitor dashboard
      </h1>

      <TabBar tabs={tabs} />

      {isStatsTab ? <MonitorPanel /> : <ErrorLogTab />}
    </div>
  );
}
