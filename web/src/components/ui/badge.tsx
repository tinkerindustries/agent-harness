import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import { Slot } from "radix-ui"

import { cn } from "@/lib/utils"

// The status variants swap background as well as colour when an outcome flips,
// so background-color has to be in the transition list; the duration matches
// --dur-swap and the easing --ease-out. The label crossfade itself comes from
// .anim-badge-in on the element, applied by the caller.
const badgeVariants = cva(
  "inline-flex w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-full border border-transparent px-2 py-0.5 text-xs font-medium whitespace-nowrap transition-[color,background-color,box-shadow] duration-[220ms] ease-[cubic-bezier(0.16,1,0.3,1)] focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 aria-invalid:border-destructive aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40 [&>svg]:pointer-events-none [&>svg]:size-3",
  {
    variants: {
      variant: {
        default: "bg-primary text-primary-foreground [a&]:hover:bg-primary/90",
        secondary:
          "bg-secondary text-secondary-foreground [a&]:hover:bg-secondary/90",
        destructive:
          "bg-destructive text-white focus-visible:ring-destructive/20 dark:bg-destructive/60 dark:focus-visible:ring-destructive/40 [a&]:hover:bg-destructive/90",
        outline:
          "border-border text-foreground [a&]:hover:bg-accent [a&]:hover:text-accent-foreground",
        ghost: "[a&]:hover:bg-accent [a&]:hover:text-accent-foreground",
        link: "text-primary underline-offset-4 [a&]:hover:underline",
        // The harness's own status vocabulary on top of the shadcn badge
        // (docs/WEB-REDESIGN.md phase 1): the class names carry the
        // --status-* tints defined in src/styles.css, ported verbatim from
        // design/tokens.css. Uppercase matches what the pre-shadcn
        // .status-badge rendered.
        running: "badge-running uppercase",
        done: "badge-done uppercase",
        gaveup: "badge-gaveup uppercase",
        stopped: "badge-stopped uppercase",
        failed: "badge-failed uppercase",
        restart: "badge-restart uppercase",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function Badge({
  className,
  variant = "default",
  asChild = false,
  ...props
}: React.ComponentProps<"span"> &
  VariantProps<typeof badgeVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot.Root : "span"

  return (
    <Comp
      data-slot="badge"
      data-variant={variant}
      className={cn(badgeVariants({ variant }), className)}
      {...props}
    />
  )
}

export { Badge, badgeVariants }
