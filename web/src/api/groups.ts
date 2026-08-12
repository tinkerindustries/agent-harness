import type { Block } from "./fold";
import type { Todo } from "./types";

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

// GroupTags is the filter classification of one sub-turn group, computed in
// the same pass that builds the group (docs/WEB-REDESIGN.md phase 5: the
// chip counts "come from the same pass that renders them"). Only the tail
// group is ever created or replaced, so the walk stays bounded to one
// sub-turn's blocks.
export interface GroupTags {
  // edits counts Edit and Write tool results in the group — the "edits"
  // filter family (design/transcript.html groups both under one glyph
  // colour).
  edits: number;
  // bash counts Bash tool results.
  bash: number;
  // errors counts failed tool results (is_error) and denied calls — the
  // cards that must stay open in Compact mode.
  errors: number;
  // churn is whether the group's usage carried a churn_point_index.
  churn: boolean;
}

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
  // tags is the group's filter classification, computed from blocks and
  // usage the moment the group object is created or replaced.
  tags: GroupTags;
  // phase is the plan item the sub-turn ran under (docs/WEB-REDESIGN.md
  // phase 6): captured the moment the group is created, so every group but
  // the tail one carries the same ref forever, and the rail can group the
  // sub-turns without walking the session's TaskCreate/TaskUpdate history
  // itself.
  phase: RailPhaseRef;
}

// RailPhaseRef is a sub-turn's phase membership for the timeline rail
// (docs/WEB-REDESIGN.md phase 6): the plan item that was in_progress when
// the sub-turn ran. A new phase starts on every TaskCreate or TaskUpdate
// call in the event stream — the plan mutations the fold already applies
// (its latestTodos is the plan as of the last such call), so
// SubTurnGroupState only has to notice the call in the group's own assistant
// block. The label/index are captured from the fold's latestTodos at that
// moment, never re-parsed here.
export interface RailPhaseRef {
  // id is a monotonically increasing phase identifier, stable for the
  // session; the rail's Accordion items and the observer's current marker
  // key on it.
  id: number;
  // index is the 1-based position in the plan of the item that was
  // in_progress ("1 · Fix retained-body leak"). 0 when the plan at the
  // boundary had no usable item (no plan mutation yet, or an empty plan).
  index: number;
  // label is that plan item's subject, without the "1 · " prefix the rail
  // renders. Empty when there was no plan yet or no usable item.
  label: string;
}

// phaseFromTodos picks the plan item a new phase is named after: the item
// that was in_progress, falling back to the first non-completed one when the
// plan marked nothing in_progress (a freshly written plan commonly leaves
// everything pending). A plan whose every item is completed names the phase
// after its last item — the final TaskUpdate leaves nothing in_progress, but
// the sub-turns that follow still belong to the plan. Empty only when the
// plan had no item to name the phase.
export function phaseFromTodos(todos: Todo[], id: number): RailPhaseRef {
  let index = -1;
  let subject = "";
  for (let i = 0; i < todos.length; i++) {
    if (todos[i].status === "in_progress") {
      index = i;
      subject = todos[i].subject;
      break;
    }
  }
  if (index < 0) {
    for (let i = 0; i < todos.length; i++) {
      if (todos[i].status !== "completed") {
        index = i;
        subject = todos[i].subject;
        break;
      }
    }
  }
  if (index < 0 && todos.length > 0) {
    index = todos.length - 1;
    subject = todos[index].subject;
  }
  return index < 0 ? { id, index: 0, label: "" } : { id, index: index + 1, label: subject };
}

// ChurnPoint is the first sub-turn whose usage carried a cache-churn
// diagnostic, for the banner above the transcript (docs/WEB-REDESIGN.md
// phase 5): sub-turn, and the tokens re-sent above the expected miss.
export interface ChurnPoint {
  subTurn: number;
  excessTokens: number;
}

// GroupCounts is the chip row's numbers: how many sub-turn cards match each
// filter family, maintained incrementally in the same pass that builds the
// groups — never a second walk over the blocks per chip.
export interface GroupCounts {
  total: number;
  edits: number;
  bash: number;
  errors: number;
  churn: number;
}

// TranscriptFilter is the filter chip vocabulary: which cards the transcript
// shows. "all" is the no-filter state.
export type TranscriptFilter = "all" | "edits" | "bash" | "errors" | "churn";

const ZERO_TAGS: GroupTags = { edits: 0, bash: 0, errors: 0, churn: false };

// computeTags classifies a group from its own blocks and usage.
export function computeTags(group: Pick<SubTurnGroup, "blocks" | "usage">): GroupTags {
  const tags: GroupTags = { ...ZERO_TAGS };
  for (const block of group.blocks) {
    if (block.type === "tool_result") {
      if (block.name === "Edit" || block.name === "Write") tags.edits++;
      else if (block.name === "Bash") tags.bash++;
      if (block.is_error) tags.errors++;
    } else if (block.type === "tool_denied") {
      tags.errors++;
    }
  }
  if (group.usage?.churn_point_index !== undefined) tags.churn = true;
  return tags;
}

// withTags returns the group with its tags computed. Only called on a group
// being created or replaced — i.e. the tail group.
function withTags(group: Omit<SubTurnGroup, "tags">): SubTurnGroup {
  return { ...group, tags: computeTags(group) };
}

// groupMatchesFilter is the chips' membership test: a group matches "edits"
// when it contains an Edit/Write result, "bash" when it contains a Bash
// result, "errors" when it contains a failed result or a denial, and "churn"
// when its usage carried a churn diagnostic. The tags it reads were computed
// when the group was built, so matching costs no block walk at render time.
export function groupMatchesFilter(group: SubTurnGroup, filter: TranscriptFilter): boolean {
  switch (filter) {
    case "all":
      return true;
    case "edits":
      return group.tags.edits > 0;
    case "bash":
      return group.tags.bash > 0;
    case "errors":
      return group.tags.errors > 0;
    case "churn":
      return group.tags.churn;
  }
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
  // counts and churnPoint are maintained in the same pass that builds
  // items, so the chip row and the churn banner read them off the snapshot
  // instead of walking the blocks again (docs/WEB-REDESIGN.md phase 5).
  counts: GroupCounts = { total: 0, edits: 0, bash: 0, errors: 0, churn: 0 };
  churnPoint: ChurnPoint | null = null;
  // The phase timeline (docs/WEB-REDESIGN.md phase 6): currentPhase is the
  // plan item the next group to freeze ran under, bumped on every TaskCreate
  // or TaskUpdate call in a frozen assistant's own toolCalls (the two plan
  // mutations — TaskGet/TaskList are reads and never start a phase). The
  // phase's name comes from the fold's already-applied plan, passed in per
  // sync — so nothing here parses a plan tool's arguments. latestTodos is
  // the fallback plan (the one the store held at flush time); todosAtBlock
  // is the plan as of each block index, recorded by the store while it
  // folded the events, which is what names a phase correctly when a flush
  // folds a whole batch of blocks at once (a replay burst): with only the
  // batch-end plan, every phase in the batch would be named from the last
  // plan mutation in it.
  private phaseSeq = 0;
  private currentPhase: RailPhaseRef = { id: 0, index: 0, label: "" };
  private latestTodos: Todo[] = [];
  private todosAtBlock: Todo[][] = [];

  // sync folds the current blocks array into items incrementally. Called on
  // every store snapshot; the fast path (same array reference — a live-only
  // delta, the token-rate hot path) returns the same items array unchanged.
  // latestTodos is the fold's plan as of this snapshot and todosAtBlock the
  // plan as of each block index (see the field comment) — the boundary
  // sub-turn's own plan names the new phase the moment its group is created.
  sync(blocks: Block[], latestTodos: Todo[] = [], todosAtBlock: Todo[][] = []): TranscriptItem[] {
    if (blocks === this.lastBlocks) return this.items;
    this.latestTodos = latestTodos;
    this.todosAtBlock = todosAtBlock;
    if (this.lastBlocks !== null && blocks.length <= this.lastBlocks.length) {
      // Same length, different reference: FoldState replaced an element in
      // place. Two replacements exist: attachReasoningTokens, which always
      // rides the ingest that appended the usage block, and the steer
      // block's pending → delivered flip, which rides the ingest of its
      // steer_applied event. Both are handled by refreshAmended below.
      this.refreshAmended(blocks);
      this.lastBlocks = blocks;
      return this.items;
    }
    const start = this.lastBlocks ? this.lastBlocks.length : 0;
    for (let i = start; i < blocks.length; i++) this.pushBlock(blocks[i], blocks, i);
    this.lastBlocks = blocks;
    return this.items;
  }

  // pushBlock folds one completed block into the grouped view, in the same
  // style FoldState.pushBlock uses: an assistant block starts a new group,
  // tool results and usage append to the last one, and everything else stays
  // top-level. blocks is the whole current array, needed only so a usage
  // block can re-read its group's (possibly amended) assistant element;
  // blockIndex is the block's position in that array, which picks the plan
  // (todosAtBlock) the boundary sub-turn wrote.
  private pushBlock(block: Block, blocks: Block[], blockIndex: number): void {
    switch (block.type) {
      case "assistant":
        // A TaskCreate or TaskUpdate in the sub-turn's own calls marks a new
        // phase (docs/WEB-REDESIGN.md phase 6): the boundary is free — every
        // plan-mutating call in the event stream starts one, while the
        // TaskGet/TaskList reads do not — and the fold's already-applied
        // plan, as of this block (todosAtBlock), names it. The group below
        // freezes with that phase forever.
        if (block.toolCalls.some((c) => c.name === "TaskCreate" || c.name === "TaskUpdate")) {
          this.phaseSeq++;
          this.currentPhase = phaseFromTodos(this.todosAt(blockIndex), this.phaseSeq);
        }
        this.addGroup(withTags({ subTurn: block.subTurn, seq: block.seq, blocks: [block], phase: this.currentPhase }));
        break;
      case "tool_result":
      case "tool_denied": {
        const last = this.lastGroup();
        if (!last) {
          this.items = [...this.items, { kind: "block", block }];
          break;
        }
        this.replaceGroup(last.index, withTags({ ...last.group, blocks: [...last.group.blocks, block] }));
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
          this.observeChurn(block);
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
        this.observeChurn(block);
        this.replaceGroup(last.index, withTags({ ...group, blocks: children, usage: block }));
        break;
      }
      default:
        // opening, skills, run_finished, error, steer — top-level, outside
        // any group. A steer block later flips pending → delivered in place
        // (refreshAmended); the other top-level blocks freeze once and are
        // never touched again.
        this.items = [...this.items, { kind: "block", block }];
        break;
    }
  }

  // addGroup appends a brand-new group and accounts its tags in counts.
  private addGroup(group: SubTurnGroup): void {
    this.items = [...this.items, { kind: "group", group }];
    this.addCounts(group.tags);
  }

  private lastGroup(): { index: number; group: SubTurnGroup } | null {
    for (let i = this.items.length - 1; i >= 0; i--) {
      const item = this.items[i];
      if (item.kind === "group") return { index: i, group: item.group };
    }
    return null;
  }

  // todosAt is the plan as of one block index: the store's per-block record
  // when there is one, otherwise the plan it held at the latest sync (the
  // per-event callers — tests, groupBySubTurn — pass no timeline and get the
  // same naming as before).
  private todosAt(blockIndex: number): Todo[] {
    return this.todosAtBlock[blockIndex] ?? this.latestTodos;
  }

  // replaceGroup swaps one item in place. Only the tail group is ever
  // replaced, so counts are maintained by subtracting the outgoing group's
  // membership and adding the incoming one's — an O(1) bookkeeping cost per
  // append, not a walk over the session.
  private replaceGroup(index: number, group: SubTurnGroup): void {
    const prev = this.items[index];
    if (prev && prev.kind === "group") this.subtractCounts(prev.group.tags);
    this.addCounts(group.tags);
    this.items = [...this.items];
    this.items[index] = { kind: "group", group };
  }

  // membership is a group's contribution to counts: the filter families are
  // per-card (does this card contain an edit?), so a card with two Edits
  // counts once for the edits chip, matching what the filter shows.
  private static membership(tags: GroupTags): GroupCounts {
    return {
      total: 1,
      edits: tags.edits > 0 ? 1 : 0,
      bash: tags.bash > 0 ? 1 : 0,
      errors: tags.errors > 0 ? 1 : 0,
      churn: tags.churn ? 1 : 0,
    };
  }

  private addCounts(tags: GroupTags): void {
    const m = SubTurnGroupState.membership(tags);
    this.counts = {
      total: this.counts.total + m.total,
      edits: this.counts.edits + m.edits,
      bash: this.counts.bash + m.bash,
      errors: this.counts.errors + m.errors,
      churn: this.counts.churn + m.churn,
    };
  }

  private subtractCounts(tags: GroupTags): void {
    const m = SubTurnGroupState.membership(tags);
    this.counts = {
      total: this.counts.total - m.total,
      edits: this.counts.edits - m.edits,
      bash: this.counts.bash - m.bash,
      errors: this.counts.errors - m.errors,
      churn: this.counts.churn - m.churn,
    };
  }

  // observeChurn records the first churn diagnostic seen, whether the usage
  // lands in a group's header or stays a loose block (a starved retry's
  // first usage). The banner links to the first churn sub-turn; later ones
  // still count on the chip.
  private observeChurn(usage: UsageBlock): void {
    if (this.churnPoint || usage.churn_point_index === undefined) return;
    this.churnPoint = {
      subTurn: usage.sub_turn,
      excessTokens: Math.max(0, usage.prompt_cache_miss_tokens - usage.expected_miss_tokens),
    };
  }

  // refreshAmended handles an in-place element replacement in the blocks
  // array without an accompanying append. Two cases exist: the assistant
  // block amended by attachReasoningTokens (a sub-turn card's first child),
  // and — since phase 5 — a steer block flipped pending → delivered by its
  // steer_applied event (a top-level block, which the fold keeps at the
  // position where the operator sent it). The scan is defensive; in practice
  // only the tail item is ever affected.
  private refreshAmended(blocks: Block[]): void {
    const prev = this.lastBlocks!;
    for (let i = 0; i < blocks.length; i++) {
      if (blocks[i] === prev[i]) continue;
      const b = blocks[i];
      if (b.type === "assistant") {
        for (let j = this.items.length - 1; j >= 0; j--) {
          const item = this.items[j];
          if (item.kind === "group" && item.group.subTurn === b.subTurn && item.group.blocks[0] !== b) {
            this.replaceGroup(j, { ...item.group, blocks: [b, ...item.group.blocks.slice(1)] });
            return;
          }
        }
        return;
      }
      if (b.type === "steer") {
        for (let j = 0; j < this.items.length; j++) {
          const item = this.items[j];
          if (item.kind === "block" && item.block.type === "steer" && item.block.seq === b.seq && item.block !== b) {
            this.items = [...this.items];
            this.items[j] = { kind: "block", block: b };
            return;
          }
        }
        return;
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
