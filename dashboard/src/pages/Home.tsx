import { Link } from "react-router-dom";
import { useVersion } from "../data/useVersion";

export default function Home() {
  const version = useVersion();

  return (
    <div
      style={{
        padding: "2rem",
        fontFamily: "'IBM Plex Sans', sans-serif",
      }}
    >
      <h1
        style={{
          display: "flex",
          alignItems: "center",
          fontSize: "2rem",
          fontWeight: 700,
          color: "#111",
          margin: 0,
          letterSpacing: "-0.03em",
        }}
      >
        <img
          src="favicon.svg"
          alt=""
          style={{ height: "1em", width: "1em", marginRight: "0.5rem" }}
        />
        Sprinkles
        {version && (
          <span
            style={{
              fontSize: "0.85rem",
              fontWeight: 400,
              color: "#bbb",
              letterSpacing: "normal",
              marginLeft: "0.6rem",
            }}
          >
            {version}
          </span>
        )}
        <Link
          to="/jobs"
          style={{
            fontSize: "0.8rem",
            fontWeight: 400,
            color: "#1565c0",
            letterSpacing: "normal",
            marginLeft: "1rem",
            textDecoration: "none",
          }}
        >
          Jobs
        </Link>
        <Link
          to="/workpools"
          style={{
            fontSize: "0.8rem",
            fontWeight: 400,
            color: "#1565c0",
            letterSpacing: "normal",
            marginLeft: "1rem",
            textDecoration: "none",
          }}
        >
          Work Pools
        </Link>
        <Link
          to="/errors"
          style={{
            fontSize: "0.8rem",
            fontWeight: 400,
            color: "#1565c0",
            letterSpacing: "normal",
            marginLeft: "1rem",
            textDecoration: "none",
          }}
        >
          Error Log
        </Link>
      </h1>
    </div>
  );
}
