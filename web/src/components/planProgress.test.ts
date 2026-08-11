import { describe, expect, it } from "vitest";
import { planProgress, splitVerb } from "./planProgress";
import type { Todo } from "../api/types";

let nextId = 0;

function todo(partial: Partial<Todo> & Pick<Todo, "content">): Todo {
  nextId++;
  // The fixtures never assert on ids — planProgress reads content/status —
  // so mint them from a counter, the same shape TaskCreate produces.
  return { id: String(nextId), status: "pending", activeForm: "", ...partial };
}

// The collapsed in-flight line and the finished table's subtitle both read
// the plan through planProgress (docs/WEB-REDESIGN.md phase 3): done counts
// completed items only, and the active form is the in_progress item's — the
// "6 / 11" and "Wiring six call sites…" of design/sessions.html.
describe("planProgress", () => {
  it("counts completed items over the whole plan", () => {
    const todos = [
      todo({ content: "a", status: "completed" }),
      todo({ content: "b", status: "completed" }),
      todo({ content: "c", status: "in_progress", activeForm: "Wiring c" }),
      todo({ content: "d" }),
      todo({ content: "e" }),
    ];
    expect(planProgress(todos)).toEqual({ done: 2, total: 5, activeForm: "Wiring c" });
  });

  it("prefers the in_progress activeForm over an earlier pending item", () => {
    const todos = [
      todo({ content: "first, read" }),
      todo({ content: "then wire", status: "in_progress", activeForm: "Wiring call sites" }),
    ];
    expect(planProgress(todos).activeForm).toBe("Wiring call sites");
  });

  it("falls back to the first pending item before any is in progress", () => {
    const todos = [todo({ content: "read the spec" }), todo({ content: "wire it up" })];
    expect(planProgress(todos)).toEqual({ done: 0, total: 2, activeForm: "read the spec" });
  });

  it("returns an empty active form and zero ratio for an empty plan", () => {
    expect(planProgress([])).toEqual({ done: 0, total: 0, activeForm: "" });
  });

  it("a plan with every item done has an empty active form", () => {
    const todos = [todo({ content: "a", status: "completed" })];
    expect(planProgress(todos)).toEqual({ done: 1, total: 1, activeForm: "" });
  });
});

describe("splitVerb", () => {
  it("splits the leading verb from the rest of the sentence", () => {
    expect(splitVerb("Wiring six call sites")).toEqual({ verb: "Wiring", rest: "six call sites" });
  });

  it("a single word has no rest", () => {
    expect(splitVerb("Wiring")).toEqual({ verb: "Wiring", rest: "" });
  });
});
