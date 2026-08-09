import { useEffect, useState } from "react";
import { SessionListScreen } from "./components/SessionListScreen";
import { TranscriptScreen } from "./components/TranscriptScreen";
import { PerfHarnessScreen } from "./perf/PerfHarnessScreen";

// Two screens, no router library (docs/DESIGN.md §5.7): plain pathname
// parsing plus history.pushState/popstate. "/" is the session list;
// "/sessions/:id" is one session's transcript. The Go static handler falls
// back to index.html for any unrecognised path, so a reload or a direct
// link to /sessions/:id still loads this app and lands on the right
// screen. "/perf" is the phase 5 measurement harness (web/src/perf) — a
// developer tool, not part of the read-only product surface, but routed
// here rather than as a second Vite entry point so it exercises the exact
// same build and component tree the real transcript does.

type Route = { kind: "list" } | { kind: "session"; id: string } | { kind: "perf" };

function parseRoute(pathname: string): Route {
  if (pathname.replace(/\/$/, "") === "/perf") return { kind: "perf" };
  const m = pathname.match(/^\/sessions\/([^/]+)\/?$/);
  return m ? { kind: "session", id: decodeURIComponent(m[1]) } : { kind: "list" };
}

export default function App() {
  const [route, setRoute] = useState<Route>(() => parseRoute(window.location.pathname));

  useEffect(() => {
    const onPopState = () => setRoute(parseRoute(window.location.pathname));
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  function navigate(path: string) {
    window.history.pushState({}, "", path);
    setRoute(parseRoute(path));
  }

  if (route.kind === "session") {
    return <TranscriptScreen sessionId={route.id} onBack={() => navigate("/")} />;
  }
  if (route.kind === "perf") {
    return <PerfHarnessScreen />;
  }
  return <SessionListScreen onOpen={(id) => navigate(`/sessions/${encodeURIComponent(id)}`)} />;
}
