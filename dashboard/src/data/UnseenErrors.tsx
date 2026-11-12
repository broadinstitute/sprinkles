import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import type { ReactNode } from "react";
import { apiFetch } from "../api/client";

const STORAGE_KEY = "sprinkles.errorLog.lastSeen";
const POLL_INTERVAL_MS = 30_000;

// The stored value is a *server* timestamp (epoch ms of the newest error the
// user has seen), never the browser's clock, so skew between the two can't
// cause false alarms or missed ones.
function readSeen(): number | null {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    const n = v === null ? NaN : Number(v);
    return Number.isFinite(n) ? n : null;
  } catch {
    return null;
  }
}

function writeSeen(ms: number) {
  try {
    localStorage.setItem(STORAGE_KEY, String(ms));
  } catch {
    /* storage unavailable; the badge just won't persist */
  }
}

interface UnseenErrorsValue {
  /** True when the server has an error newer than the last one the user saw. */
  hasUnseen: boolean;
  /** Record that the user has seen errors up to this server timestamp. */
  markSeen: (timestamp: string) => void;
}

const UnseenErrorsContext = createContext<UnseenErrorsValue>({
  hasUnseen: false,
  markSeen: () => {},
});

export function UnseenErrorsProvider({ children }: { children: ReactNode }) {
  const [latestMs, setLatestMs] = useState<number | null>(null);
  const [seenMs, setSeenMs] = useState<number | null>(readSeen);

  useEffect(() => {
    let cancelled = false;
    async function poll() {
      while (!cancelled) {
        try {
          const res = await apiFetch("/api/v1/errors/latest");
          if (res.ok && !cancelled) {
            const data: { latest_timestamp: string | null } = await res.json();
            setLatestMs(
              data.latest_timestamp ? Date.parse(data.latest_timestamp) : null,
            );
          }
        } catch {
          // transient; retry next tick
        }
        await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
      }
    }
    poll();
    return () => {
      cancelled = true;
    };
  }, []);

  // Keep other tabs in sync: viewing the log in one clears the badge in all.
  useEffect(() => {
    function onStorage(e: StorageEvent) {
      if (e.key === STORAGE_KEY) setSeenMs(readSeen());
    }
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  const markSeen = useCallback((timestamp: string) => {
    const ms = Date.parse(timestamp);
    if (!Number.isFinite(ms)) return;
    setSeenMs((prev) => {
      if (prev !== null && prev >= ms) return prev;
      writeSeen(ms);
      return ms;
    });
  }, []);

  const value = useMemo<UnseenErrorsValue>(
    () => ({
      // Nothing stored yet counts as "unseen" if any error exists.
      hasUnseen: latestMs !== null && (seenMs === null || latestMs > seenMs),
      markSeen,
    }),
    [latestMs, seenMs, markSeen],
  );

  return (
    <UnseenErrorsContext.Provider value={value}>
      {children}
    </UnseenErrorsContext.Provider>
  );
}

export function useUnseenErrors(): UnseenErrorsValue {
  return useContext(UnseenErrorsContext);
}
