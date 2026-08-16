import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { ArrowLeft, ChartLineUp, GearSix, Stack, Wrench } from "@phosphor-icons/react";
import type { Route } from "../App";
import { cn } from "@/lib/utils";

// The shared top nav: the wordmark, the sections as real links with
// an active state, and — on a session
// detail — a crumb for the session id. One component, mounted once
// by App around whichever screen the route renders; the screens never
// redeclare it. A session detail is the exception to the links: it is a
// page you arrived at from a list and leave by going back, so the section
// links give way to one back link to the session list — the run's own
// controls own the rest of the row. What the design keeps page-specific — the session list's
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
      <header
        className={cn(
          "mx-auto mt-4 mb-5 flex max-w-[960px] items-center gap-1 border-b border-border px-4 py-2.5 max-phone:px-3 max-phone:py-1 max-xs:px-2",
          (route.kind === "session" || route.kind === "evalRun") && "max-w-[1200px]",
          // A session page runs its bands to the viewport's edges; the nav
          // has to do the same or its rule stops short of them.
          route.kind === "session" && "topnav-flush max-w-none mx-0",
          // A session page stays sticky at the top so a long, ordinarily-
          // scrolling transcript (body.page — SessionScreen.tsx) never
          // scrolls the nav (and its Stop control) out of reach; harmless
          // when the same route is in the fixed app-shell (body.app)
          // instead, since nothing there scrolls past the header at all.
          route.kind === "session" && "sticky top-0 z-30 bg-background",
        )}
      >
        <span
          className={cn(
            "mr-2.5 font-mono text-[0.875rem] font-semibold whitespace-nowrap tracking-[-0.01em]",
            "max-phone:mr-1.5 max-phone:text-xs max-xs:mr-1 max-xs:text-micro",
            // A session page already carries its own context in the back
            // link and the crumb, so the brand mark is the one thing a
            // phone-width session page doesn't have room to keep.
            route.kind === "session" && "max-phone:hidden",
          )}
        >
          agent-harness
        </span>
        {route.kind === "session" ? (
          <nav className="flex items-center gap-0.5">
            <a
              className="inline-flex h-7 items-center gap-1.5 rounded-md py-0 pr-2.5 pl-1.5 text-sm font-medium text-muted-foreground no-underline hover:bg-accent hover:text-foreground max-nav:px-2 max-phone:h-11 max-phone:px-1.5 max-xs:px-[3px]"
              href="/"
              onClick={(e) => {
                e.preventDefault();
                onNavigate("/");
              }}
              title="Back to sessions"
            >
              <ArrowLeft aria-hidden className="h-3.5 w-3.5 shrink-0" />
              <span className="max-nav:sr-only">Sessions</span>
            </a>
          </nav>
        ) : (
          <nav className="flex items-center gap-0.5">
            <NavLink
              label="Sessions"
              icon={<Stack aria-hidden className="hidden h-3.5 w-3.5 shrink-0 max-nav:inline" />}
              href="/"
              active={section === "sessions"}
              onNavigate={onNavigate}
            />
            <NavLink
              label="Evals"
              icon={<ChartLineUp aria-hidden className="hidden h-3.5 w-3.5 shrink-0 max-nav:inline" />}
              href="/evals"
              active={section === "evals"}
              onNavigate={onNavigate}
            />
            <NavLink
              label="Operations"
              icon={<Wrench aria-hidden className="hidden h-3.5 w-3.5 shrink-0 max-nav:inline" />}
              href="/operations"
              active={section === "operations"}
              onNavigate={onNavigate}
            />
            <NavLink
              label="Settings"
              icon={<GearSix aria-hidden className="hidden h-3.5 w-3.5 shrink-0 max-nav:inline" />}
              href="/settings"
              active={section === "settings"}
              onNavigate={onNavigate}
            />
          </nav>
        )}
        {(route.kind === "session" || route.kind === "evalRun") && (
          <span className="flex min-w-0 items-center gap-1.5 text-sm text-muted-foreground max-nav:min-w-0 max-nav:flex-1">
            <span className="shrink-0 text-border">/</span>
            <span className="min-w-0 overflow-hidden font-mono whitespace-nowrap text-ellipsis text-foreground">
              {route.id}
            </span>
          </span>
        )}
        <span className="flex-1" />
        <div className="flex items-center gap-2 max-nav:min-w-0 max-nav:flex-nowrap max-nav:overflow-x-auto max-nav:[scrollbar-width:none] max-nav:[&::-webkit-scrollbar]:hidden topnav-right">
          {right}
        </div>
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
  icon,
  href,
  active,
  onNavigate,
}: {
  label: string;
  icon: ReactNode;
  href: string;
  active: boolean;
  onNavigate: (path: string) => void;
}) {
  return (
    <a
      className={cn(
        "inline-flex h-7 items-center gap-1.5 rounded-md px-2.5 text-sm font-medium text-muted-foreground no-underline hover:bg-accent hover:text-foreground max-nav:px-2 max-phone:h-11 max-phone:px-1.5 max-xs:px-[3px]",
        active && "bg-secondary text-foreground",
      )}
      href={href}
      title={label}
      aria-current={active ? "page" : undefined}
      onClick={(e) => {
        e.preventDefault();
        onNavigate(href);
      }}
    >
      {icon}
      <span className="max-nav:sr-only">{label}</span>
    </a>
  );
}
