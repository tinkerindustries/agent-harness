// The models client: GET /api/models, the read-only list of model names the
// harness knows (docs/DATA-API.md "models", internal/provider). The start
// forms' model dropdowns are fed from here rather than a hardcoded copy, so
// a model added to the provider table shows up in the browser without a
// frontend change — the same lesson as TestPromptNamesExactlyTheToolArray: a
// second copy is a second place to drift, and this repository has been
// bitten by exactly that once already.
//
// The forms need two things beyond the list: which model to preselect, and
// what to render when the request fails. Both come from the settings
// registry (the harness's own configuration), so this module also carries
// the two helpers that read the model-naming rows. The endpoint itself has
// nothing to fail — the table is compiled in — so the only failure here is
// the fetch, and the fallback is deliberately the settings rows rather than
// a hardcoded name: a failed request degrades to what the harness is
// configured to run, never to a frontend copy.

import type { SettingEntry } from "./settings";

// The two settings keys whose values name the forms' default models. The
// harness resolves an empty work-request model to model.default
// (internal/worker, cmd/harness), and model.flash is what Task subagents
// and the eval form default to. Both are read through settingModel below, so
// an operator's override of either key changes what the forms preselect.
export const DEFAULT_MODEL_KEY = "model.default";
export const FLASH_MODEL_KEY = "model.flash";

// ModelsResponse mirrors internal/httpapi.modelsResponse.
export interface ModelsResponse {
  models: string[];
}

// listModels fetches GET /api/models. The names come back sorted — the same
// order every other consumer of the provider table sees.
export async function listModels(): Promise<string[]> {
  const res = await fetch("/api/models");
  if (!res.ok) throw await apiError(res);
  const body = (await res.json()) as ModelsResponse;
  return body.models;
}

// settingModel reads one model-naming setting's effective value: the stored
// override when the key is set, the registry default otherwise, empty when
// the settings fetch failed entirely (no row for the key at all).
export function settingModel(entries: SettingEntry[], key: string): string {
  const entry = entries.find((e) => e.key === key);
  return entry?.value ?? entry?.default ?? "";
}

// resolveModelOptions is the dropdown's option list: the endpoint's models
// when the fetch landed, and when it failed the settings' two model defaults
// (model.default and model.flash, deduplicated) — so a failed request
// renders the models the harness is configured to run, never an empty
// dropdown. When the settings fetch failed too the list is empty; the forms
// then send no model, which the harness resolves to model.default, and show
// a hint instead of pretending.
export function resolveModelOptions(models: string[] | null, entries: SettingEntry[]): string[] {
  if (models && models.length > 0) return models;
  const fromSettings = [
    settingModel(entries, DEFAULT_MODEL_KEY),
    settingModel(entries, FLASH_MODEL_KEY),
  ].filter((m) => m !== "");
  return [...new Set(fromSettings)];
}

// apiError turns a non-2xx models response into a readable Error. The
// server writes every error as {"error": "..."} (docs/DATA-API.md "Error
// shape"), so the message can go straight to the form's hint next to the
// dropdown.
async function apiError(res: Response): Promise<Error> {
  let message = `${res.status} ${res.statusText}`;
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) message = body.error;
  } catch {
    // A non-JSON error body falls back to the status line.
  }
  return new Error(message);
}
