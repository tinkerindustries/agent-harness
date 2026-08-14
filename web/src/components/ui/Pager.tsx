import { clampPage, offsetFor, pageCount } from "../../api/paging";
import { cn } from "@/lib/utils";
import { Button } from "./button";

// Pager is the reusable control under a paged table: Prev, "Page X of Y",
// Next, and the row range this page is showing out of the total. Both
// buttons are disabled at their ends (Prev on page 1, Next on the last
// page) and carry aria-labels. It renders nothing when there is one page
// or none — a pager with nothing to page is noise, and the table's own
// empty state already says what an empty result means.
//
// className lets a caller carry its own spacing — the default .pager rule
// has a margin-top that is wrong for a copy sitting above a table. label,
// when given, rides on the control as its aria-label, so a screen reader
// user meeting two Prev/Next pairs on one page can tell them apart.
export function Pager({
  page,
  total,
  perPage,
  onPage,
  className,
  label,
}: {
  page: number;
  total: number;
  perPage: number;
  onPage: (page: number) => void;
  className?: string;
  label?: string;
}) {
  const count = pageCount(total, perPage);
  if (count <= 1) return null;
  const safe = clampPage(page, total, perPage);
  const first = offsetFor(safe, perPage) + 1;
  const last = Math.min(safe * perPage, total);
  return (
    <div className={cn("pager", className)} aria-label={label}>
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
