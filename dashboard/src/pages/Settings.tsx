import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { apiFetch } from "../api/client";
import type { SprinklesConfig } from "../types";

const MONO = "'IBM Plex Mono', monospace";

// One row per SprinklesConfig field, in display order. Keep in sync with
// SprinklesConfig in cli/dev/dashboard_backend.go.
const FIELDS: {
  key: keyof SprinklesConfig;
  label: string;
  description: string;
}[] = [
  {
    key: "gcsPrefix",
    label: "GCS prefix",
    description: "Where task result and log paths are written.",
  },
  {
    key: "subscriberSA",
    label: "Subscriber service account",
    description:
      "Used to mint short-lived Pub/Sub tokens for the subscription endpoint.",
  },
  {
    key: "sprinklesWorkerGCSPath",
    label: "Worker binary path",
    description:
      "Default GCS path to the worker binary, used when a workpool omits one.",
  },
  {
    key: "serviceAccount",
    label: "Worker service account",
    description: "Default service account worker VMs run as.",
  },
  {
    key: "region",
    label: "Region",
    description: "Default region worker VMs are provisioned in.",
  },
  {
    key: "zones",
    label: "Zones",
    description: "Default zones eligible for worker VM placement.",
  },
];

function formatValue(v: string | string[] | undefined): string {
  if (Array.isArray(v)) return v.length ? v.join(", ") : "(none)";
  return v ? v : "(not set)";
}

export default function Settings() {
  const [config, setConfig] = useState<SprinklesConfig>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await apiFetch("/api/v1/config");
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data: SprinklesConfig = await res.json();
        if (!cancelled) setConfig(data);
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div style={{ maxWidth: 900, margin: "0 auto", padding: "2rem 1.5rem" }}>
      <div style={{ fontFamily: MONO, fontSize: "0.8rem", marginBottom: 12 }}>
        <Link to="/" style={{ color: "#1565c0", textDecoration: "none" }}>
          ← Home
        </Link>
      </div>
      <h1 style={{ fontFamily: MONO, fontSize: "1.4rem", margin: "0 0 4px" }}>
        Settings
      </h1>
      <p style={{ color: "#666", fontSize: "0.85rem", margin: "0 0 1.25rem" }}>
        Read-only view of the backend&apos;s SprinklesConfig.
      </p>
      {error && (
        <div style={{ color: "#b71c1c", fontFamily: MONO }}>
          Failed to load settings: {error}
        </div>
      )}
      {!config && !error && <div style={{ color: "#666" }}>Loading…</div>}
      {config && (
        <div style={{ border: "1px solid #e0e3e6", borderRadius: 8 }}>
          {FIELDS.map((f, i) => (
            <div
              key={f.key}
              style={{
                padding: "12px 16px",
                borderTop: i === 0 ? "none" : "1px solid #e0e3e6",
              }}
            >
              <div style={{ fontWeight: 600, fontSize: "0.85rem" }}>
                {f.label}
              </div>
              <div
                style={{
                  fontFamily: MONO,
                  fontSize: "0.85rem",
                  margin: "3px 0",
                  wordBreak: "break-all",
                }}
              >
                {formatValue(config[f.key])}
              </div>
              <div style={{ color: "#777", fontSize: "0.75rem" }}>
                {f.description}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
