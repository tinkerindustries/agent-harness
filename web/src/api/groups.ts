import type { Block } from "./fold";

// The sub-turn grouping (docs/WEB-REDESIGN.md phase 4): a pure display-side
// view over the fold's `blocks` array that groups each sub-turn's assistant
// block, tool results, and usage into one card. It is deliberately NOT a new
// Block variant — the Block union and the event fold stay exactly as they
// are (web/CLAUDE.md: "src/api/fold.ts must stay in shape agreement with
// internal/fold"), and internal/fold never learns this module exists.
//
// Like FoldState, the incremental path matters more than the pure one: a
// session's blocks array only ever grows (pushBlock) or has one element
// amended in place (attachReasoningTokens, which rides the same ingest that
// appends the usage block). SubTurnGroupState.sync exploits exactly that —
// reference-identical array, return the same items; otherwise fold only the
// newly appended blocks onto the tail. Every group but the tail one keeps
// its object — and therefore its children array — reference-identical
// forever, which is what lets the SubTurnCard memo bail out for all of them
// (docs/DESIGN.md §5.2's freeze, at group granularity).

export type UsageBlock = Extract<Block, { type: "usage" }>;

export interface SubTurnGroup {
  // The sub-turn number, from the assistant block that opens the group.
  subTurn: number;
  // The opening assistant block's seq — a stable React key for the card.
  seq: number;
  // The group's blocks in order: the assistant block, then each of its
  // tool_result / tool_denied blocks as they froze. Reference-stable from
  // the moment the group's last block lands.
  blocks: Block[];
  // The sub-turn's usage block, absorbed into the card header instead of
  // rendering as a sibling block. At most one per group: a starved retry's
  // first usage arrives before its assistant block freezes, so it cannot be
  // absorbed here and stays a loose block; the retry's own usage (the last
  // one) is what lands in the header.
  usage?: UsageBlock;
}

// TranscriptItem is one entry of the grouped transcript: either a sub-turn
// group (a card) or a block that stays top-level, outside any group —
// opening, skills, run_finished, error, and any usage whose sub-turn has no
// group yet.
export type TranscriptItem =
  | { kind: "group"; group: SubTurnGroup }
  | { kind: "block"; block: Block };

export class SubTurnGroupState {
  private items: TranscriptItem[] = [];
  private lastBlocks: Block[] | null = null;

  // sync folds the current blocks array into items incrementally. Called on
  // every store snapshot; the fast path (same array reference — a live-only
  // delta, the token-rate hot path) returns the same items array unchanged.
  sync(blocks: Block[]): TranscriptItem[] {
    if (blocks === this.lastBlocks) return this.items;
    if (this.lastBlocks !== null && blocks.length <= this.lastBlocks.length) {
      // Same length, different reference: FoldState replaced an element in
      // place. The only such replacement is attachReasoningTokens, which
      // always rides the ingest that appended the usage block, so the append
      // path below already re-reads the amended assistant. Nothing to do
      // here beyond remembering the new array; the defensive refresh exists
      // in case some future fold change replaces an element on its own.
      this.refreshAmended(blocks);
      this.lastBlocks = blocks;
      return this.items;
    }
    const start = this.lastBlocks ? this.lastBlocks.length : 0;
    for (let i = start; i < blocks.length; i++) this.pushBlock(blocks[i], blocks);
    this.lastBlocks = blocks;
    return this.items;
  }

  // pushBlock folds one completed block into the grouped view, in the same
  // style FoldState.pushBlock uses: an assistant block starts a new group,
  // tool results and usage append to the last one, and everything else stays
  // top-level. blocks is the whole current array, needed only so a usage
  // block can re-read its group's (possibly amended) assistant element.
  private pushBlock(block: Block, blocks: Block[]): void {
    switch (block.type) {
      case "assistant":
        this.items = [...this.items, { kind: "group", group: { subTurn: block.subTurn, seq: block.seq, blocks: [block] } }];
        break;
      case "tool_result":
      case "tool_denied": {
        const last = this.lastGroup();
        if (!last) {
          this.items = [...this.items, { kind: "block", block }];
          break;
        }
        this.replaceGroup(last.index, { ...last.group, blocks: [...last.group.blocks, block] });
        break;
      }
      case "usage": {
        const last = this.lastGroup();
        // Only absorb a usage whose sub-turn matches the group it is being
        // absorbed into. In the normal event order the usage for a sub-turn
        // arrives right after its tool results, so the last group matches;
        // a starved retry's first usage arrives before its assistant block
        // freezes and must not leak into the previous turn's header.
        if (!last || last.group.subTurn !== block.sub_turn) {
          this.items = [...this.items, { kind: "block", block }];
          break;
        }
        const group = last.group;
        // The usage ingest also amended the group's assistant in place
        // (attachReasoningTokens on the same event). Re-read it from the
        // blocks array so the card renders the amended element — the API's
        // reasoning_tokens figure rather than the character estimate.
        let assistant = group.blocks[0];
        for (let i = blocks.length - 1; i >= 0; i--) {
          const b = blocks[i];
          if (b.type === "assistant" && b.subTurn === block.sub_turn) {
            assistant = b;
            break;
          }
        }
        const children = assistant === group.blocks[0] ? group.blocks : [assistant, ...group.blocks.slice(1)];
        this.replaceGroup(last.index, { ...group, blocks: children, usage: block });
        break;
      }
      default:
        // opening, skills, run_finished, error — top-level, outside any group.
        this.items = [...this.items, { kind: "block", block }];
        break;
    }
  }

  private lastGroup(): { index: number; group: SubTurnGroup } | null {
    for (let i = this.items.length - 1; i >= 0; i--) {
      const item = this.items[i];
      if (item.kind === "group") return { index: i, group: item.group };
    }
    return null;
  }

  private replaceGroup(index: number, group: SubTurnGroup): void {
    this.items = [...this.items];
    this.items[index] = { kind: "group", group };
  }

  // refreshAmended handles the (currently unreachable) case of an in-place
  // element replacement without an accompanying append: swap the amended
  // assistant block into its group's children so the card shows the new
  // element. Other groups are left untouched.
  private refreshAmended(blocks: Block[]): void {
    const prev = this.lastBlocks!;
    for (let i = 0; i < blocks.length; i++) {
      if (blocks[i] === prev[i]) continue;
      const b = blocks[i];
      if (b.type !== "assistant") return;
      for (let j = this.items.length - 1; j >= 0; j--) {
        const item = this.items[j];
        if (item.kind === "group" && item.group.subTurn === b.subTurn && item.group.blocks[0] !== b) {
          this.replaceGroup(j, { ...item.group, blocks: [b, ...item.group.blocks.slice(1)] });
          return;
        }
      }
      return;
    }
  }
}

// groupBySubTurn folds a whole blocks array at once, for callers with no
// need for incremental updates — tests, and rebuilding a one-off view. Live
// sessions use SubTurnGroupState.sync directly from TranscriptStore so a
// burst of events costs at most one fold, not one group pass per event —
// the same split fold.ts draws between foldEvents and FoldState.
export function groupBySubTurn(blocks: Block[]): TranscriptItem[] {
  const state = new SubTurnGroupState();
  return state.sync(blocks);
}
