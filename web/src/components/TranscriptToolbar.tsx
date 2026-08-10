import { Toggle } from "./ui/toggle";
import { ToggleGroup, ToggleGroupItem } from "./ui/toggle-group";
import { cn } from "@/lib/utils";
import type { GroupCounts, TranscriptFilter } from "../api/groups";
import type { Density } from "./blocks/SubTurnCard";

// TranscriptToolbar is the transcript's control strip (design/transcript.html's
// .toolbar, docs/WEB-REDESIGN.md phase 5): the Compact/Full density toggle on
// the left, then one filter chip per family with its card count. The counts
// come straight off the snapshot's GroupCounts — the same pass that builds
// the cards — so the toolbar never walks the blocks itself. This is display
// state only: nothing here starts, steers, or stops a run.
interface Props {
  density: Density;
  onDensityChange: (density: Density) => void;
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

export function TranscriptToolbar({ density, onDensityChange, filter, onFilterChange, counts }: Props) {
  return (
    <div className="transcript-toolbar" role="toolbar" aria-label="Transcript">
      <ToggleGroup
        type="single"
        variant="outline"
        size="sm"
        value={density}
        onValueChange={(value) => {
          if (value === "compact" || value === "full") onDensityChange(value);
        }}
        aria-label="Density"
      >
        <ToggleGroupItem value="compact">Compact</ToggleGroupItem>
        <ToggleGroupItem value="full">Full</ToggleGroupItem>
      </ToggleGroup>
      <span className="toolbar-sep" aria-hidden />
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
