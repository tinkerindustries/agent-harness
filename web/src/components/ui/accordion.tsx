import * as React from "react"
import { Accordion as AccordionPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

// shadcn/ui's Accordion (docs/WEB-REDESIGN.md phase 1's component list, and
// components.html's mapping of details.rail-phase to "Accordion (type
// multiple)"), written against the unified radix-ui package like the rest of
// this ui directory. The rail's phase groups are several-open-at-once, which
// is exactly what type="multiple" gives; the trigger/content/header
// primitives are styled in the bespoke layer (styles.css) rather than here,
// because the rail's summary row is its own layout (caret, phase name,
// sub-turn range) — see .rail-phase.
//
// Deliberately no chevron icon: the rail's phases draw the design's ▸ caret
// themselves.

function Accordion({
  ...props
}: React.ComponentProps<typeof AccordionPrimitive.Root>) {
  return <AccordionPrimitive.Root data-slot="accordion" {...props} />
}

function AccordionItem({
  className,
  ...props
}: React.ComponentProps<typeof AccordionPrimitive.Item>) {
  return (
    <AccordionPrimitive.Item
      data-slot="accordion-item"
      className={cn("border-b last:border-b-0", className)}
      {...props}
    />
  )
}

function AccordionTrigger({
  className,
  children,
  ...props
}: React.ComponentProps<typeof AccordionPrimitive.Trigger>) {
  return (
    <AccordionPrimitive.Header className="flex">
      <AccordionPrimitive.Trigger
        data-slot="accordion-trigger"
        className={cn("flex flex-1 items-center", className)}
        {...props}
      >
        {children}
      </AccordionPrimitive.Trigger>
    </AccordionPrimitive.Header>
  )
}

function AccordionContent({
  className,
  children,
  ...props
}: React.ComponentProps<typeof AccordionPrimitive.Content>) {
  return (
    <AccordionPrimitive.Content
      data-slot="accordion-content"
      className={cn(
        // No height animation: the rail is a scroll surface on a frame-budget
        // sensitive screen, and a phase can hold a couple of dozen entries.
        // Radix's data-state toggle still mounts/unmounts the rows.
        "grid overflow-hidden text-sm data-[state=closed]:grid-rows-[0fr] data-[state=open]:grid-rows-[1fr]",
        className
      )}
      {...props}
    >
      <div className="min-w-0 overflow-hidden">{children}</div>
    </AccordionPrimitive.Content>
  )
}

export { Accordion, AccordionContent, AccordionItem, AccordionTrigger }
