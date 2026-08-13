import type { Todo } from "../api/types";

// PlanProgress is what the collapsed in-flight line answers — a collapsed
// card answers what the session is doing and how far in it is: how many
// plan items are done of how many, and the activeForm of the item in
// progress — or, before any
// item is in progress, the subject of the first pending one, so the line
// has something to say from the moment a plan is written.
export interface PlanProgress {
  done: number;
  total: number;
  activeForm: string;
}

// planProgress turns a plan into the collapsed line's ratio and active
// form. done counts only completed items (the finished table's "11 of 11
// plan items" counts the same way); an in_progress item wins the active
// form over any pending one regardless of position.
export function planProgress(todos: Todo[]): PlanProgress {
  let done = 0;
  let activeForm = "";
  let nextSubject = "";
  for (const t of todos) {
    if (t.status === "completed") {
      done++;
    } else if (t.status === "in_progress" && activeForm === "") {
      activeForm = t.activeForm;
    } else if (t.status === "pending" && nextSubject === "") {
      nextSubject = t.subject;
    }
  }
  return { done, total: todos.length, activeForm: activeForm || nextSubject };
}

// splitVerb splits the collapsed line's "what it is doing" text into the
// leading verb, which the drawing colours with the running tint, and the
// rest of the sentence (.run-now .verb).
export function splitVerb(text: string): { verb: string; rest: string } {
  const i = text.indexOf(" ");
  if (i === -1) return { verb: text, rest: "" };
  return { verb: text.slice(0, i), rest: text.slice(i + 1) };
}
