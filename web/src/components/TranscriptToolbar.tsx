import { Toggle } from "./ui/toggle";
import { cn } from "@/lib/utils";
import type { GroupCounts, TranscriptFilter } from "../api/groups";

// TranscriptToolbar is the filter chip row for the child-transcript block
// vocabulary and the perf harnesses: one chip per filter family with its
// card count. A turn is always full — there is no Compact/Full density
// toggle — and the collapsing happens per tool row instead. The counts come
// straight off the snapshot's GroupCounts — the same pass that builds
// the turns — so the toolbar never walks the blocks itself. This is
// display state only: the run-control stop lives in the
// screen header (StopControl), and nothing here starts, steers, or stops a
// run. The watch page's own rail carries its chips separately; this
// toolbar is not mounted there.
interface Props {
  filter: TranscriptFilter;
  onFilterChange: (filter: TranscriptFilter) => void;
  counts: GroupCounts;
}

const FILTERS: { key: TranscriptFilter; label: string }[] = [
  { key: "all", label: "All" },
  { key: "edits", label: "Edits" },
  { key: "bash", label: "Bash" },
  { key: "errors", label: "Errors" },
  { key: "churn", label: "Churn" },
];

export function TranscriptToolbar({ filter, onFilterChange, counts }: Props) {
  return (
    <div
      className="sticky top-0 z-20 my-3.5 flex flex-wrap items-center gap-2 border-y border-border bg-background py-2.5"
      role="toolbar"
      aria-label="Transcript"
    >
      {FILTERS.map(({ key, label }) => (
        <Toggle
          key={key}
          pressed={filter === key}
          // The chips are radio-like: pressing one selects it, and pressing
          // the selected one again keeps it selected (there is no "no
          // filter" state other than All). Radix already reflects that as
          // data-state="on", so the active look reads off that rather than
          // a second filter === key comparison.
          onPressedChange={() => onFilterChange(key)}
          className={cn(
            "h-[26px] min-w-0 gap-[5px] rounded-full border border-border bg-background px-2.5 text-xs text-muted-foreground hover:bg-muted data-[state=on]:border-ring data-[state=on]:bg-secondary data-[state=on]:text-foreground",
          )}
        >
          {label} {filterCount(counts, key)}
        </Toggle>
      ))}
    </div>
  );
}

function filterCount(counts: GroupCounts, key: TranscriptFilter): number {
  switch (key) {
    case "all":
      return counts.total;
    case "edits":
      return counts.edits;
    case "bash":
      return counts.bash;
    case "errors":
      return counts.errors;
    case "churn":
      return counts.churn;
  }
}
