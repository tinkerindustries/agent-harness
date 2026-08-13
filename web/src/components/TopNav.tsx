import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import type { Route } from "../App";
import { cn } from "@/lib/utils";

// The shared top nav: the wordmark, the three sections as real links with
// an active state, and — on a session
// detail — a crumb for the session id. One component, mounted once
// by App around whichever screen the route renders; the screens never
// redeclare it. What the design keeps page-specific — the session list's
// search input and LIVE badge, the settings screen's search input, the
// operations screen's refresh button, the transcript's connection badge —
// is rendered by the owning screen into the right slot through useNavRight,
// so the nav stays shared while the content stays with the screen that holds
// its state.

// The nav's right slot, as a context: TopNav owns the slot's DOM, the
// screens own what goes in it. useNavRight re-registers the node whenever
// the screen re-renders with a new one, so a ticking list (the elapsed
// second) keeps the badge current without any of it living in App.
const NavRightContext = createContext<(node: ReactNode) => void>(() => {});

export function useNavRight(node: ReactNode): void {
  const setRight = useContext(NavRightContext);
  useEffect(() => {
    setRight(node);
    return () => setRight(null);
  }, [setRight, node]);
}

// section is which of the three links the current route belongs to: the
// session list and a session's transcript are both the Sessions section,
// and only the transcript additionally renders the crumb below, in the
// slot a page title would otherwise sit in.
function sectionOf(route: Route): "sessions" | "evals" | "operations" | "settings" {
  if (route.kind === "list" || route.kind === "session") return "sessions";
  if (route.kind === "evals" || route.kind === "evalRun") return "evals";
  if (route.kind === "operations") return "operations";
  return "settings";
}

interface TopNavProps {
  route: Route;
  onNavigate: (path: string) => void;
  // The screen the route renders, mounted by App as the nav's child: the
  // nav wraps the screen so the NavRightContext provider above it reaches
  // the screen, which registers
  // its page-specific right-hand content through useNavRight.
  children: ReactNode;
}

export function TopNav({ route, onNavigate, children }: TopNavProps) {
  const [right, setRight] = useState<ReactNode>(null);
  const section = sectionOf(route);

  return (
    <NavRightContext.Provider value={setRight}>
      <header className={cn("topnav", (route.kind === "session" || route.kind === "evalRun") && "topnav-wide")}>
        <span className="wordmark">deepseek-harness</span>
        <nav className="topnav-links">
          <NavLink label="Sessions" href="/" active={section === "sessions"} onNavigate={onNavigate} />
          <NavLink label="Evals" href="/evals" active={section === "evals"} onNavigate={onNavigate} />
          <NavLink
            label="Operations"
            href="/operations"
            active={section === "operations"}
            onNavigate={onNavigate}
          />
          <NavLink
            label="Settings"
            href="/settings"
            active={section === "settings"}
            onNavigate={onNavigate}
          />
        </nav>
        {(route.kind === "session" || route.kind === "evalRun") && (
          <span className="nav-crumb">
            <span className="sep">/</span>
            <span className="current">{route.id}</span>
          </span>
        )}
        <span className="spacer" />
        <div className="topnav-right">{right}</div>
      </header>
      {children}
    </NavRightContext.Provider>
  );
}

// NavLink is one section as a real link (middle-click and a reload both
// land, the Go static handler falls back to index.html) with an SPA
// click-through: navigate() is pushState, so following a link never reloads
// the app the way the design's flat HTML pages do.
function NavLink({
  label,
  href,
  active,
  onNavigate,
}: {
  label: string;
  href: string;
  active: boolean;
  onNavigate: (path: string) => void;
}) {
  return (
    <a
      className={cn("navlink", active && "navlink-active")}
      href={href}
      aria-current={active ? "page" : undefined}
      onClick={(e) => {
        e.preventDefault();
        onNavigate(href);
      }}
    >
      {label}
    </a>
  );
}
