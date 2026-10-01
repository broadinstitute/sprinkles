import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useEvents } from "../data/EventProvider";
import { useVersion } from "../data/useVersion";
import { apiFetch } from "../api/client";
import type { WorkPoolListEntry } from "../types";

const TERMINAL_JOB_STATES = new Set(["success", "error", "failed", "killed"]);
const UNHEALTHY_POOL_STATES = new Set(["halted", "unhealthy"]);
const POLL_INTERVAL_MS = 10_000;

function useWorkPools(): WorkPoolListEntry[] | undefined {
  const [workpools, setWorkpools] = useState<WorkPoolListEntry[]>();
  useEffect(() => {
    let cancelled = false;
    async function poll() {
      while (!cancelled) {
        try {
          const res = await apiFetch("/api/v1/workpools");
          if (res.ok) {
            const data: WorkPoolListEntry[] = await res.json();
            if (!cancelled) setWorkpools(data);
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
  }, []);
  return workpools;
}

export default function Home() {
  const version = useVersion();
  const { jobs } = useEvents();
  const workpools = useWorkPools();

  const activeJobs = useMemo(
    () => jobs.filter((j) => !TERMINAL_JOB_STATES.has(j.state)).length,
    [jobs],
  );
  const activePools = workpools?.filter((p) => p.state !== "idle").length;
  const unhealthyPools =
    workpools?.filter((p) => UNHEALTHY_POOL_STATES.has(p.state)).length ?? 0;

  return (
    <>
      <style>{styles}</style>
      <div className="home-root">
        <div className="home-header">
          <img src="favicon.svg" alt="" className="home-logo" />
          <span className="home-title">Sprinkles</span>
          {version && <span className="home-version">{version}</span>}
          {workpools && (
            <span className="home-status">
              <span
                className={`home-status-dot${
                  unhealthyPools > 0 ? " home-status-dot-bad" : ""
                }`}
              />
              {unhealthyPools > 0
                ? `${unhealthyPools} work pool${
                    unhealthyPools !== 1 ? "s" : ""
                  } need${unhealthyPools === 1 ? "s" : ""} attention`
                : "All systems operational"}
            </span>
          )}
        </div>

        <div className="home-cards">
          <Link to="/jobs" className="home-card">
            <div className="home-card-label">Jobs</div>
            <div className="home-card-value">{activeJobs}</div>
            <div className="home-card-caption">active jobs</div>
          </Link>
          <Link to="/workpools" className="home-card">
            <div className="home-card-label">Work Pools</div>
            <div className="home-card-value">{activePools ?? "—"}</div>
            <div className="home-card-caption">active workpools</div>
          </Link>
          <Link to="/errors" className="home-card home-card-link">
            Error Log →
          </Link>
        </div>

        <div className="home-callouts">
          <div className="home-callout">
            <strong>What&apos;s a job?</strong>
            User-submitted work made up of one or more tasks. Every job has a
            workpool associated with it.
          </div>
          <div className="home-callout">
            <strong>What&apos;s a workpool?</strong>A dynamically provisioned
            collection of identically configured instances. Executes tasks as
            its capacity allows.
          </div>
        </div>
      </div>
    </>
  );
}

// Colors, fonts and spacing follow the Broad Clinical Labs design system
// tokens used by the landing-page mockup.
const styles = `
  .home-root {
    --bcl-blue: #006DB6;
    --bcl-gray: #63666A;
    --bcl-gray-bg: #F2F4F6;
    --bcl-ink: #1A1D1F;
    --bcl-border: #E0E3E6;
    --bcl-green: #80BC42;
    --bcl-negative: #C0554A;

    max-width: 1040px;
    margin: 0 auto;
    box-sizing: border-box;
    padding: 36px 40px 0;
    color: var(--bcl-ink);
    font-family: 'Open Sans', sans-serif;
  }

  .home-header {
    display: flex;
    align-items: center;
    gap: 14px;
    margin-bottom: 28px;
  }

  .home-logo {
    width: 36px;
    height: 36px;
    object-fit: contain;
  }

  .home-title {
    font: 700 30px/1 'IBM Plex Mono', monospace;
    color: #1a1a1a;
  }

  .home-version {
    font: 400 12px 'IBM Plex Mono', monospace;
    color: #fff;
    background: var(--bcl-gray);
    border-radius: 3px;
    padding: 3px 7px;
  }

  .home-status {
    margin-left: auto;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font: 400 12px 'Open Sans', sans-serif;
    color: var(--bcl-gray);
  }

  .home-status-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--bcl-green);
    display: inline-block;
  }

  .home-status-dot-bad { background: var(--bcl-negative); }

  .home-cards {
    display: grid;
    grid-template-columns: 1fr 1fr 220px;
    gap: 16px;
    margin-bottom: 24px;
  }

  .home-card {
    display: block;
    padding: 18px 20px;
    border: 1px solid var(--bcl-border);
    border-radius: 8px;
    background: #fff;
    text-decoration: none;
    transition: border-color 0.12s;
  }

  .home-card:hover { border-color: var(--bcl-blue); }

  .home-card-label {
    font: 600 12px 'Open Sans', sans-serif;
    color: var(--bcl-gray);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    margin-bottom: 6px;
  }

  .home-card-value {
    font: 700 32px/1 Roboto, sans-serif;
    color: var(--bcl-blue);
  }

  .home-card-caption {
    font: 400 12px 'Open Sans', sans-serif;
    color: var(--bcl-gray);
    margin-top: 4px;
  }

  .home-card-link {
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--bcl-gray-bg);
    font: 600 14px 'Open Sans', sans-serif;
    color: #1a1a1a;
  }

  .home-callouts {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 16px;
    margin-bottom: 28px;
  }

  .home-callout {
    background: var(--bcl-gray-bg);
    border-left: 4px solid var(--bcl-blue);
    border-radius: 4px;
    padding: 16px;
    font: 400 16px 'Open Sans', sans-serif;
  }

  .home-callout strong {
    display: block;
    font-family: Roboto, sans-serif;
    margin-bottom: 4px;
  }
`;
