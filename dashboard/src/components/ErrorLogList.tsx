import { useLayoutEffect, useRef, useState } from "react";
import type { Ref } from "react";
import type { ErrorLogEntry } from "../types";

/** Messages longer than this many lines are collapsed until expanded. */
const COLLAPSED_LINES = 3;

function ErrorEntry({ entry, last }: { entry: ErrorLogEntry; last: boolean }) {
  const [expanded, setExpanded] = useState(false);
  const [truncated, setTruncated] = useState(false);
  const msgRef = useRef<HTMLSpanElement>(null);

  // Only offer the toggle if the clamped message is actually cut off. Measured
  // while collapsed (scrollHeight > clientHeight); once expanded we keep the
  // last result so the "Show less" button doesn't vanish.
  useLayoutEffect(() => {
    const el = msgRef.current;
    if (!el || expanded) return;
    const check = () => setTruncated(el.scrollHeight > el.clientHeight + 1);
    check();
    const ro = new ResizeObserver(check);
    ro.observe(el);
    return () => ro.disconnect();
  }, [entry.message, expanded]);

  return (
    <div
      style={{
        display: "flex",
        gap: "1rem",
        padding: "6px 16px",
        borderBottom: last ? "none" : "1px solid #3a3f4b",
        whiteSpace: "pre-wrap",
      }}
    >
      <span style={{ color: "#5c6370", whiteSpace: "nowrap" }}>
        {new Date(entry.timestamp).toLocaleString()}
      </span>
      <div style={{ flex: 1, minWidth: 0 }}>
        <span
          ref={msgRef}
          style={{
            color: "#e06c75",
            overflowWrap: "anywhere",
            ...(expanded
              ? { display: "block" }
              : {
                  display: "-webkit-box",
                  WebkitBoxOrient: "vertical",
                  WebkitLineClamp: COLLAPSED_LINES,
                  overflow: "hidden",
                }),
          }}
        >
          {entry.message}
        </span>
        {(truncated || expanded) && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            style={{
              background: "none",
              border: "none",
              padding: 0,
              marginTop: 2,
              color: "#61afef",
              cursor: "pointer",
              font: "inherit",
              fontSize: "0.75rem",
            }}
          >
            {expanded ? "Show less" : "Show more"}
          </button>
        )}
      </div>
    </div>
  );
}

/** The scrolling, terminal-style list of monitor errors. */
export default function ErrorLogList({
  errors,
  loading = false,
  bottomRef,
}: {
  errors: ErrorLogEntry[];
  loading?: boolean;
  /** Optional sentinel at the end of the list, for auto-scrolling. */
  bottomRef?: Ref<HTMLDivElement>;
}) {
  return (
    <div
      style={{
        border: "1px solid #e0e0e0",
        borderRadius: 8,
        background: "#282c34",
        height: "70vh",
        overflowY: "auto",
        fontSize: "0.82rem",
        lineHeight: 1.6,
        boxSizing: "border-box",
      }}
    >
      {loading ? (
        <div style={{ padding: "1rem", color: "#5c6370", fontStyle: "italic" }}>
          Loading…
        </div>
      ) : errors.length === 0 ? (
        <div style={{ padding: "1rem", color: "#5c6370", fontStyle: "italic" }}>
          No errors recorded.
        </div>
      ) : (
        errors.map((e, i) => (
          <ErrorEntry
            key={`${e.timestamp}-${i}`}
            entry={e}
            last={i === errors.length - 1}
          />
        ))
      )}
      <div ref={bottomRef} />
    </div>
  );
}
