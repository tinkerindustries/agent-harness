// Shared Tailwind for the "boxed, labelled" rendering FrozenBlock's per-kind
// components (AssistantBlock, MiscBlocks, ToolDeniedBlock, ToolResultBlock,
// LiveBlocks) give a loose top-level block — the card frame the old .block
// rule gave every one of them, the small-caps header line .block-label gave
// every label div, and the pre-wrapped body text .block-text/.block-pre gave
// their content. Centralised because roughly ten components share these byte
// for byte; copying the string out to each would just be the same value ten
// times over, with room for one to drift the way the codebase's genuinely
// per-file residuals (.think/.tool) are allowed to because each only has one
// or two consumers.
//
// BLOCK_CLS carries only the frame (radius, border width, padding) and
// deliberately omits border-color and background: half its consumers keep
// the plain --border/--card pair, the other half (an error border, a denied
// border, steer's dashed accent) replace them outright, and baking a color
// in here would mean every one of those had to fight it with a second
// same-property utility instead of just stating the one it wants.
// .block-opening/.block-skills build their own frame from scratch instead of
// starting from this one: both replace every property .block sets, so
// nothing here would survive into them regardless.
export const BLOCK_CLS = "rounded-[calc(var(--radius)-4px)] border p-2";
export const BLOCK_LABEL_CLS = "mb-1 text-xs text-muted-foreground uppercase";
export const BLOCK_TEXT_CLS = "m-0 whitespace-pre-wrap";
export const BLOCK_PRE_CLS =
  "m-0 max-h-[400px] overflow-auto whitespace-pre-wrap text-[0.85rem] max-phone:text-[0.875rem]";

// The loose tool-call fallback AssistantBlock/LiveAssistantBlock render for a
// call whose result has not landed inside a sub-turn card — Turn.tsx's own
// tool rows are a different, later-converted rendering with no equivalent.
export const TOOL_CALL_CLS = "mt-1 text-[0.85rem]";
// The target a tool call named, dimmed relative to the block label around it
// (ToolResultBlock, ToolDeniedBlock, AssistantBlock, LiveBlocks,
// ScreenshotGallery's no-session fallback, TaskChildTranscript's toggle).
export const TOOL_DETAIL_CLS = "text-muted-foreground font-normal normal-case";
