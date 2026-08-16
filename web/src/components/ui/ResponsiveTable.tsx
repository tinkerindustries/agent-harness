import type { ReactNode, TableHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

// A dense data table that becomes stacked cards below the phone breakpoint:
// rows turn into cards (RTRow), the header becomes screen-reader-only
// (RTHead), and each cell prints its own visible label as real DOM text
// (RTCell) instead of a CSS `content: attr(data-label)` pseudo-element keyed
// to the cell's `:nth-child` position. Column position drove the label and
// the mobile flex-basis in the three tables this replaces (finished
// sessions, evals, operations) — fragile to any change in column order or
// count elsewhere. Here every cell carries its own explicit classes.

export function RTTable({ className, children, ...props }: TableHTMLAttributes<HTMLTableElement>) {
  return (
    <table className={cn("w-full border-collapse text-[0.9rem] max-phone:block", className)} {...props}>
      {children}
    </table>
  );
}

export function RTHead({ children }: { children: ReactNode }) {
  return <thead className="max-phone:sr-only">{children}</thead>;
}

export function RTBody({ children }: { children: ReactNode }) {
  return <tbody className="max-phone:block">{children}</tbody>;
}

export function RTTh({ className, children }: { className?: string; children?: ReactNode }) {
  return (
    <th className={cn("whitespace-nowrap border-b border-border px-2 py-1.5 text-left font-semibold text-muted-foreground", className)}>
      {children}
    </th>
  );
}

export function RTRow({
  className,
  onClick,
  children,
}: {
  className?: string;
  onClick?: () => void;
  children: ReactNode;
}) {
  return (
    <tr
      className={cn(
        "hover:bg-muted max-phone:mb-2.5 max-phone:flex max-phone:flex-wrap max-phone:gap-x-3 max-phone:gap-y-1 max-phone:rounded-md max-phone:border max-phone:border-border max-phone:bg-card max-phone:p-2.5",
        onClick && "cursor-pointer",
        className,
      )}
      onClick={onClick}
    >
      {children}
    </tr>
  );
}

// No default white-space here — callers opt into `whitespace-nowrap` per
// cell (a short numeric figure) rather than it being the base, so a long
// id/path cell wraps instead of forcing the table wider than its container.
const cellBase = "border-b border-border px-2 py-1.5 align-top max-phone:whitespace-normal max-phone:border-none max-phone:p-0";

// The leading, unlabeled cell — a status badge or icon, sized to its own
// content and pinned to the card's top-left on mobile.
export function RTLead({ className, children }: { className?: string; children: ReactNode }) {
  return <td className={cn(cellBase, "max-phone:basis-auto max-phone:self-start max-phone:pt-0.5", className)}>{children}</td>;
}

// The rich, unlabeled cell — a title/description block. Takes the rest of
// the lead cell's row and pushes every other cell onto lines below it.
export function RTMain({ className, children }: { className?: string; children: ReactNode }) {
  return <td className={cn(cellBase, "max-phone:min-w-0 max-phone:grow max-phone:basis-[78%]", className)}>{children}</td>;
}

// A labeled figure cell. `wide` spans the card's full width (a model name);
// otherwise it takes half, so figures pair up two-per-line the way the
// desktop columns read left to right.
export function RTCell({
  label,
  wide = false,
  className,
  title,
  children,
}: {
  label: string;
  wide?: boolean;
  className?: string;
  title?: string;
  children: ReactNode;
}) {
  return (
    <td
      data-label={label}
      title={title}
      className={cn(cellBase, wide ? "max-phone:basis-full" : "max-phone:basis-[calc(50%-6px)]", className)}
    >
      <span className="hidden max-phone:mb-px max-phone:block max-phone:text-[11px] max-phone:tracking-[0.06em] max-phone:text-muted-foreground max-phone:uppercase">
        {label}
      </span>
      {children}
    </td>
  );
}

// The trailing, unlabeled actions cell (e.g. operations' Stop/Cancel
// controls): full width on mobile, set off by a dashed rule since it holds
// controls rather than one of the row's data figures.
export function RTActions({ className, children }: { className?: string; children: ReactNode }) {
  return (
    <td
      className={cn(
        cellBase,
        "max-phone:mt-1 max-phone:flex max-phone:basis-full max-phone:flex-col max-phone:items-start max-phone:gap-1.5 max-phone:border-t max-phone:border-dashed max-phone:border-border max-phone:pt-2",
        className,
      )}
    >
      {children}
    </td>
  );
}

// A full-width, centered placeholder row (empty state / "no match").
export function RTEmptyRow({ colSpan, children }: { colSpan: number; children: ReactNode }) {
  return (
    <tr>
      <td colSpan={colSpan} className="p-4 text-center text-muted-foreground max-phone:basis-full max-phone:border-none">
        {children}
      </td>
    </tr>
  );
}
