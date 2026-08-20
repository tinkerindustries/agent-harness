import { useEffect, useState } from "react";
import { SessionListScreen } from "./components/SessionListScreen";
import { SettingsScreen } from "./components/SettingsScreen";
import { EvalListScreen } from "./components/EvalListScreen";
import { EvalRunScreen } from "./components/EvalRunScreen";
import { OperationsScreen } from "./components/OperationsScreen";
import { MCPScreen } from "./components/MCPScreen";
import { SessionScreen } from "./components/SessionScreen";
import { TopNav } from "./components/TopNav";
import { PerfHarnessScreen } from "./perf/PerfHarnessScreen";

// Five screens, no router library (docs/DESIGN.md §5.7): plain pathname
// parsing plus history.pushState/popstate. "/" is the session list;
// "/sessions/:id" is one session's page — SessionScreen reads the row and
// forks on SessionState.parent_is_user: a run a person started renders the
// interactive chat page, a run another agent started the read-only watch
// page, both inside the full-height session shell; "/settings" is the
// settings screen; "/operations" is the operations screen — the place the
// browser writes operational state
// (closing stuck sessions, closing dead work requests, releasing stranded
// leases; docs/DATA-API.md); "/mcp-servers" is the MCP screen — the browser side of
// docs/MCP.md, where an operator registers, enables/disables, and probes the
// Model Context Protocol servers every session's tool array is built from.
// Run control is complete on the session
// surface: start (the form on the session list, docs/RUN-CONTROL.md),
// stop on the in-flight card and the session page, and steer on the
// interactive page — all through the declared seams. The Go static
// handler falls back to index.html for any unrecognised path, so a reload or
// a direct link to /sessions/:id, /settings, /operations, or /mcp-servers still loads
// this app and lands on the right screen. "/perf" is the measurement harness
// (web/src/perf) — a developer tool, not part of the read-only product
// surface, but routed here rather than as a second Vite entry point so it
// exercises the exact same build and component tree the real transcript does.
//
// The shared top nav wraps every product screen: one TopNav mounted here
// around whichever screen the route renders, not redeclared inside any of
// them. "/perf" does not get the nav — it is a developer tool, not a
// product screen.

export type Route =
  | { kind: "list" }
  | { kind: "session"; id: string }
  | { kind: "perf" }
  | { kind: "settings" }
  | { kind: "operations" }
  | { kind: "mcp" }
  | { kind: "evals" }
  | { kind: "evalRun"; id: string };

function parseRoute(pathname: string): Route {
  if (pathname.replace(/\/$/, "") === "/perf") return { kind: "perf" };
  if (pathname.replace(/\/$/, "") === "/settings") return { kind: "settings" };
  if (pathname.replace(/\/$/, "") === "/operations") return { kind: "operations" };
  // "/mcp-servers", not "/mcp": harness serve mounts its own MCP *server*
  // — the one external agent harnesses launch runs through — at "/mcp" on
  // this same port (cmd/harness/serve.go), so a screen routed there is
  // unreachable, and a browser asking for it gets that endpoint's
  // "GET requires an Mcp-Session-Id header" instead of the app. The two
  // senses of "MCP" are unrelated; only the paths collided.
  if (pathname.replace(/\/$/, "") === "/mcp-servers") return { kind: "mcp" };
  if (pathname.replace(/\/$/, "") === "/evals") return { kind: "evals" };
  const e = pathname.match(/^\/evals\/([^/]+)\/?$/);
  if (e) return { kind: "evalRun", id: decodeURIComponent(e[1]) };
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
        <SessionScreen sessionId={route.id} onNavigate={navigate} />
      ) : route.kind === "settings" ? (
        <SettingsScreen />
      ) : route.kind === "operations" ? (
        <OperationsScreen />
      ) : route.kind === "mcp" ? (
        <MCPScreen />
      ) : route.kind === "evals" ? (
        <EvalListScreen onOpen={(id) => navigate(`/evals/${encodeURIComponent(id)}`)} />
      ) : route.kind === "evalRun" ? (
        <EvalRunScreen
          id={route.id}
          onOpenSession={(id) => navigate(`/sessions/${encodeURIComponent(id)}`)}
        />
      ) : (
        <SessionListScreen onOpen={(id) => navigate(`/sessions/${encodeURIComponent(id)}`)} />
      )}
    </TopNav>
  );
}
