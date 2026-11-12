import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { apiFetch } from "../api/client";
import { RefreshToggle } from "../components/RefreshControls";
import type { ErrorLogEntry } from "../types";
import { useUnseenErrors } from "../data/UnseenErrors";
import ErrorLogList from "../components/ErrorLogList";

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

export default function ErrorLog() {
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
    <div
      style={{
        padding: "2rem",
        fontFamily: MONO,
        maxWidth: 1100,
        margin: "0 auto",
      }}
    >
      <div
        style={{
          display: "flex",
          alignItems: "baseline",
          justifyContent: "space-between",
          marginBottom: "0.5rem",
        }}
      >
        <h1
          style={{
            fontSize: "1.6rem",
            fontWeight: 700,
            color: "#111",
            margin: 0,
          }}
        >
          Error Log
        </h1>
        <div style={{ display: "flex", alignItems: "center", gap: "1rem" }}>
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
      <p
        style={{
          color: "#888",
          fontSize: "0.85rem",
          marginTop: 0,
          marginBottom: "1rem",
        }}
      >
        Recent errors from the monitor's background pollers (cluster reconciler,
        provisioning, task recovery, batch startup monitor, summary polls,
        etc.), polled every 30s.
      </p>

      <ErrorLogList errors={errors} loading={loading} bottomRef={bottomRef} />

      <div style={{ marginTop: "1.5rem" }}>
        <Link
          to="/jobs"
          style={{
            color: "#1565c0",
            textDecoration: "none",
            fontSize: "0.85rem",
          }}
        >
          ← Back to dashboard
        </Link>
      </div>
    </div>
  );
}
