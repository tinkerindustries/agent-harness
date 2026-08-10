import type { VariantProps } from "class-variance-authority";
import { badgeVariants } from "./ui/badge";

// statusVariant maps a store session status to the badge variant that
// carries its colour (docs/WEB-REDESIGN.md phase 1). The vocabulary is
// design/components.html's: done/stopped/gaveup/failed/running, with the
// store's legacy statuses folded onto them — the badge text itself is still
// the raw status, because changing what the badges say is phase 2.
type BadgeVariant = NonNullable<VariantProps<typeof badgeVariants>["variant"]>;

const STATUS_VARIANT: Record<string, BadgeVariant> = {
  running: "running",
  ok: "done",
  failed: "failed",
  timeout: "stopped",
  max_turns: "gaveup",
  cancelled: "stopped",
  compacted: "outline",
};

export function statusVariant(status: string): BadgeVariant {
  return STATUS_VARIANT[status] ?? "outline";
}
