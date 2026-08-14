// The page arithmetic the paged list controls are built from, kept pure so
// it is unit-testable (paging.test.ts): how many pages a total splits into,
// where a 1-based page number is clamped to, and which ?offset= a page asks
// for. The server clamps its own way on the wire; these helpers are the
// client's side of the same arithmetic, so the Pager and the hooks never
// each invent their own.

// pageCount is how many pages of perPage rows a total splits into. Any
// total, including zero and negative, is at least one page — an empty
// result set is one empty page, not zero pages.
export function pageCount(total: number, perPage: number): number {
  if (total <= 0) return 1;
  return Math.ceil(total / perPage);
}

// clampPage keeps page within 1..pageCount(total, perPage): never below 1,
// never above the last page, and 1 when the result set is empty. The page
// an operator is looking at can outrun the data — deleting the last row of
// the last page shrinks the total under the current page — and an empty
// table with no way back is the failure mode this exists to prevent.
export function clampPage(page: number, total: number, perPage: number): number {
  if (page < 1) return 1;
  const last = pageCount(total, perPage);
  if (page > last) return last;
  return page;
}

// offsetFor is the server-side ?offset= a 1-based page number asks for:
// page 1 is offset 0, page 2 is offset perPage, and so on.
export function offsetFor(page: number, perPage: number): number {
  return (page - 1) * perPage;
}
