import type { DiffLine } from "../../api/types";

// DiffTable renders the structured line array Go already computed
// (docs/DESIGN.md §5.4, internal/store.ComputeDiff): no diff algorithm runs
// in this render pass, just a table over data that arrived pre-aligned.
export function DiffTable({ diff }: { diff: DiffLine[] }) {
  return (
    <table className="diff-table">
      <tbody>
        {diff.map((line, i) => (
          <tr key={i} className={`diff-row diff-${line.kind}`}>
            <td className="diff-lineno">{line.old_line ?? ""}</td>
            <td className="diff-lineno">{line.new_line ?? ""}</td>
            <td className="diff-marker">{line.kind === "add" ? "+" : line.kind === "remove" ? "-" : ""}</td>
            <td className="diff-text">
              <pre>{line.text}</pre>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
