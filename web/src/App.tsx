import { useEffect, useState } from "react";
import { SessionListScreen } from "./components/SessionListScreen";
import { SettingsScreen } from "./components/SettingsScreen";
import { OperationsScreen } from "./components/OperationsScreen";
import { TranscriptScreen } from "./components/TranscriptScreen";
import { TopNav } from "./components/TopNav";
import { PerfHarnessScreen } from "./perf/PerfHarnessScreen";

// Four screens, no router library (docs/DESIGN.md §5.7): plain pathname
// parsing plus history.pushState/popstate. "/" is the session list;
// "/sessions/:id" is one session's transcript; "/settings" is the settings
// screen; "/operations" is the operations screen — the one place the browser
// writes operational state (closing stuck sessions, closing dead work
// requests, releasing stranded leases; docs/DATA-API.md phase 5). Run control
// is one action, stop — on the in-flight session card and the transcript
// header (docs/RUN-CONTROL.md); starting and steering a run from the browser
// are not built yet. The Go static
// handler falls back to index.html for any unrecognised path, so a reload or
// a direct link to /sessions/:id, /settings, or /operations still loads this
// app and lands on the right screen. "/perf" is the measurement harness
// (web/src/perf) — a developer tool, not part of the read-only product
// surface, but routed here rather than as a second Vite entry point so it
// exercises the exact same build and component tree the real transcript does.
//
// Since the redesign's phase 9, the shared top nav wraps every product
// screen (docs/WEB-REDESIGN.md): one TopNav mounted here around whichever
// screen the route renders, not redeclared inside any of them. "/perf" does
// not get the nav — it is a developer tool, not a product screen — and
// renders exactly as before.

export type Route =
  | { kind: "list" }
  | { kind: "session"; id: string }
  | { kind: "perf" }
  | { kind: "settings" }
  | { kind: "operations" };

function parseRoute(pathname: string): Route {
  if (pathname.replace(/\/$/, "") === "/perf") return { kind: "perf" };
  if (pathname.replace(/\/$/, "") === "/settings") return { kind: "settings" };
  if (pathname.replace(/\/$/, "") === "/operations") return { kind: "operations" };
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

  if (route.kind === "perf") {
    return <PerfHarnessScreen />;
  }
  return (
    <TopNav route={route} onNavigate={navigate}>
      {route.kind === "session" ? (
        <TranscriptScreen sessionId={route.id} />
      ) : route.kind === "settings" ? (
        <SettingsScreen />
      ) : route.kind === "operations" ? (
        <OperationsScreen />
      ) : (
        <SessionListScreen onOpen={(id) => navigate(`/sessions/${encodeURIComponent(id)}`)} />
      )}
    </TopNav>
  );
}
