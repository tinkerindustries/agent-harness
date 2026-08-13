import { Toggle } from "./ui/toggle";
import { cn } from "@/lib/utils";
import type { GroupCounts, TranscriptFilter } from "../api/groups";

// TranscriptToolbar is the transcript's filter chip row (docs/WEB-REDESIGN.md
// phase 5): one chip per filter family with its card count. The Compact/Full
// density toggle is gone with the session redesign — a turn is always full,
// and the collapsing happens per tool row instead. The counts come
// straight off the snapshot's GroupCounts — the same pass that builds
// the turns — so the toolbar never walks the blocks itself. This is
// display state only: the run-control stop lives in the
// screen header (StopControl), and nothing here starts, steers, or stops a
// run. Phase 4 moves the chips into the watch page's rail; until then the
// toolbar stays mounted where it is.
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
    <div className="transcript-toolbar" role="toolbar" aria-label="Transcript">
      {FILTERS.map(({ key, label }) => (
        <Toggle
          key={key}
          pressed={filter === key}
          // The chips are radio-like: pressing one selects it, and pressing
          // the selected one again keeps it selected (there is no "no
          // filter" state other than All).
          onPressedChange={() => onFilterChange(key)}
          className={cn("chip-toggle", filter === key && "chip-toggle-active")}
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
