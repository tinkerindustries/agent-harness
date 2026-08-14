import { clampPage, offsetFor, pageCount } from "../../api/paging";
import { Button } from "./button";

// Pager is the reusable control under a paged table: Prev, "Page X of Y",
// Next, and the row range this page is showing out of the total. Both
// buttons are disabled at their ends (Prev on page 1, Next on the last
// page) and carry aria-labels. It renders nothing when there is one page
// or none — a pager with nothing to page is noise, and the table's own
// empty state already says what an empty result means.
export function Pager({
  page,
  total,
  perPage,
  onPage,
}: {
  page: number;
  total: number;
  perPage: number;
  onPage: (page: number) => void;
}) {
  const count = pageCount(total, perPage);
  if (count <= 1) return null;
  const safe = clampPage(page, total, perPage);
  const first = offsetFor(safe, perPage) + 1;
  const last = Math.min(safe * perPage, total);
  return (
    <div className="pager">
      <Button
        variant="outline"
        size="sm"
        disabled={safe <= 1}
        aria-label="Previous page"
        onClick={() => onPage(safe - 1)}
      >
        Prev
      </Button>
      <span className="pager-info">
        Page {safe} of {count}
      </span>
      <Button
        variant="outline"
        size="sm"
        disabled={safe >= count}
        aria-label="Next page"
        onClick={() => onPage(safe + 1)}
      >
        Next
      </Button>
      <span className="pager-range">
        {first}–{last} of {total}
      </span>
    </div>
  );
}
