import type { DiffLine } from "../../api/types";
import { cn } from "@/lib/utils";

const lineno = "px-1.5 py-0 align-top w-[3em] text-right text-[var(--diff-gutter)] select-none";
const marker = "px-1.5 py-0 align-top w-[1em] text-center select-none";

// DiffTable renders the structured line array Go already computed
// (docs/DESIGN.md §5.4, internal/store.ComputeDiff): no diff algorithm runs
// in this render pass, just a table over data that arrived pre-aligned.
export function DiffTable({ diff }: { diff: DiffLine[] }) {
  return (
    <table className="w-full border-collapse text-[0.8rem]">
      <tbody>
        {diff.map((line, i) => (
          <tr
            key={i}
            className={cn(
              line.kind === "add" && "bg-[var(--diff-add-bg)]",
              line.kind === "remove" && "bg-[var(--diff-remove-bg)]",
            )}
          >
            <td className={lineno}>{line.old_line ?? ""}</td>
            <td className={lineno}>{line.new_line ?? ""}</td>
            <td
              className={cn(
                marker,
                line.kind === "add" && "text-[var(--diff-add-fg)]",
                line.kind === "remove" && "text-[var(--diff-remove-fg)]",
              )}
            >
              {line.kind === "add" ? "+" : line.kind === "remove" ? "-" : ""}
            </td>
            <td className="px-1.5 py-0 align-top">
              <pre className="m-0 whitespace-pre-wrap">{line.text}</pre>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
