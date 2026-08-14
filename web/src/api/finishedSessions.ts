import { useEffect, useMemo, useRef, useState } from "react";
import { useSyncExternalStore } from "react";
import { listSessions } from "./operations";
import { offsetFor } from "./paging";
import { sessionListStore } from "./sessionListStore";
import type { Page, SessionState } from "./types";

// FINISHED_PAGE_SIZE is how many finished rows one page fetches: the
// server's defaultPageLimit, asked for explicitly so the page size never
// depends on the server's default drifting.
export const FINISHED_PAGE_SIZE = 20;

// The query debounce: the filter is no longer free now that it crosses the
// network, so a keystroke burst settles into one fetch after this long of
// quiet.
const QUERY_DEBOUNCE_MS = 250;

// The revision debounce: a run finishing bumps the snapshot once, but a
// batch of finishes (several sessions closing at once, an eval run's
// members) should cost one refetch, not one per bump.
const REVISION_DEBOUNCE_MS = 500;

export interface FinishedPage {
  items: SessionState[];
  total: number;
  loading: boolean;
  error: string | null;
}

// useFinishedSessions owns the Finished table's data: one page of
// GET /api/sessions?status=finished&q=<query>&limit=20&offset=<offset> at a
// time, refetched when page or query changes (the query debounced ~250ms)
// and when the set of finished sessions changes — learnt from
// sessionListStore's finishedRevision rather than by polling, so a run that
// streams progress for an hour costs exactly the fetches that matter, and a
// run finishing or a session disappearing costs one.
//
// While a page loads, the previous page's rows stay in place: clearing the
// table between pages would flash an empty state. Live state is overlaid
// per row — any fetched row the store holds a newer copy of is replaced
// before rendering — which keeps a row's cost and status fresh between
// fetches for free, and is why the refetch signature can afford to be as
// coarse as it is.
export function useFinishedSessions(page: number, query: string): FinishedPage {
  const snapshot = useSyncExternalStore(sessionListStore.subscribe, sessionListStore.getSnapshot);

  // The query reaches the network only after it has been stable for a
  // moment. The first render uses the initial query with no wait, so the
  // first page loads immediately.
  const [debouncedQuery, setDebouncedQuery] = useState(query);
  useEffect(() => {
    const id = window.setTimeout(() => setDebouncedQuery(query), QUERY_DEBOUNCE_MS);
    return () => window.clearTimeout(id);
  }, [query]);

  // finishedRevision bumps are absorbed into a debounced copy: the fetch
  // effect keys on settledRevision, so a burst of snapshot flushes triggers
  // one refetch after the burst rather than one per bump.
  const [settledRevision, setSettledRevision] = useState(snapshot.finishedRevision);
  useEffect(() => {
    if (settledRevision === snapshot.finishedRevision) return;
    const id = window.setTimeout(() => setSettledRevision(snapshot.finishedRevision), REVISION_DEBOUNCE_MS);
    return () => window.clearTimeout(id);
  }, [settledRevision, snapshot.finishedRevision]);

  const [data, setData] = useState<Page<SessionState> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // requestRef guards against a stale response landing after a newer
  // request: only the latest request may write state.
  const requestRef = useRef(0);
  useEffect(() => {
    const request = ++requestRef.current;
    let cancelled = false;
    setLoading(true);
    setError(null);
    listSessions({
      status: "finished",
      q: debouncedQuery,
      limit: FINISHED_PAGE_SIZE,
      offset: offsetFor(page, FINISHED_PAGE_SIZE),
    })
      .then((res) => {
        if (cancelled || request !== requestRef.current) return;
        setData(res);
      })
      .catch((err) => {
        if (cancelled || request !== requestRef.current) return;
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (cancelled || request !== requestRef.current) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [debouncedQuery, page, settledRevision]);

  // The overlay source: the store's copy of each row, built from the
  // snapshot the hook already subscribes to. The stream updates live; the
  // page was fetched at some earlier moment.
  const liveById = useMemo(() => {
    const m = new Map<string, SessionState>();
    for (const s of snapshot.sessions) m.set(s.id, s);
    return m;
  }, [snapshot.sessions]);

  const items = useMemo(() => {
    if (!data) return [];
    return data.items.map((s) => liveById.get(s.id) ?? s);
  }, [data, liveById]);

  return { items, total: data?.total ?? 0, loading, error };
}
