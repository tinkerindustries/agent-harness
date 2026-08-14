// The start form's attachment handling, split out of the component so it can
// be unit-tested without a DOM (web/CLAUDE.md: components are not
// unit-tested): the file-to-payload conversion (a File into the
// {name, mime_type, data} shape POST /api/runs accepts) and the cap
// validation, with the limits read from GET /api/settings rather than
// hardcoded, so an operator who changes tools.attachments_max_count or
// tools.attachments_max_bytes sees the form change with it.

import type { SettingEntry } from "./settings";
import type { RunAttachment } from "./operations";

// ChosenAttachment is one accepted file the form keeps: the wire payload
// plus the byte size, which the payload does not carry but the chosen-file
// list shows.
export interface ChosenAttachment {
  attachment: RunAttachment;
  size: number;
}

// AttachmentCaps are the two limits POST /api/runs enforces. The server's
// registry is the source of truth (internal/settings/registry.go); this is
// the browser's copy of the effective values, read from GET /api/settings.
export interface AttachmentCaps {
  maxCount: number;
  maxBytes: number;
}

export const ATTACHMENT_MAX_COUNT_KEY = "tools.attachments_max_count";
export const ATTACHMENT_MAX_BYTES_KEY = "tools.attachments_max_bytes";

// attachmentCapsFromSettings extracts the two caps from the GET /api/settings
// rows. The effective value is the override when set, else the registry
// default — the same answer the server's resolver gives (an unset key's
// entry carries default and no value).
export function attachmentCapsFromSettings(entries: SettingEntry[]): AttachmentCaps {
  const byKey = new Map(entries.map((e) => [e.key, e]));
  const effective = (key: string): number => {
    const entry = byKey.get(key);
    if (!entry) return NaN;
    const n = Number(entry.value ?? entry.default);
    return Number.isFinite(n) ? n : NaN;
  };
  return { maxCount: effective(ATTACHMENT_MAX_COUNT_KEY), maxBytes: effective(ATTACHMENT_MAX_BYTES_KEY) };
}

// The same extension-to-MIME allowlist the tools and the server use
// (internal/tools/screenshotMIMEType): only what the vision tools (Glance,
// Ground, Detect, Crop) can read can be attached, because the model's whole
// use of the file is passing it to one of them.
const attachmentMIMEByExtension: Record<string, string> = {
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".webp": "image/webp",
};

// readAttachmentFiles turns the files the operator selected into the wire
// payload, applying the caps: at most maxCount files, each of a
// PNG/JPEG/WebP type and each at most maxBytes. A rejection is a result, not
// an exception, and names the actual limit — the form shows it next to the
// input, the same way the server's own refusal reads.
export async function readAttachmentFiles(
  files: File[],
  caps: AttachmentCaps,
): Promise<{ ok: true; chosen: ChosenAttachment[] } | { ok: false; error: string }> {
  if (files.length === 0) return { ok: true, chosen: [] };
  if (files.length > caps.maxCount) {
    return { ok: false, error: `At most ${caps.maxCount} attachments are accepted, got ${files.length}.` };
  }
  const chosen: ChosenAttachment[] = [];
  for (const file of files) {
    const mime = attachmentMIMEByExtension[extensionOf(file.name)];
    if (!mime) {
      return { ok: false, error: `${file.name} is not a PNG, JPEG, or WebP file.` };
    }
    if (file.size > caps.maxBytes) {
      return {
        ok: false,
        error: `${file.name} is ${file.size} bytes, over the ${caps.maxBytes}-byte per-file limit.`,
      };
    }
    chosen.push({ attachment: { name: file.name, mime_type: mime, data: await fileBytesToBase64(file) }, size: file.size });
  }
  return { ok: true, chosen };
}

// extensionOf returns a file name's extension, lower-cased, including the
// dot — "" when the name has none.
function extensionOf(name: string): string {
  const dot = name.lastIndexOf(".");
  return dot < 0 ? "" : name.slice(dot).toLowerCase();
}

// fileBytesToBase64 reads a File and base64-encodes its bytes. The
// String.fromCharCode(...bytes) idiom blows the call stack past a few MB,
// so the encoding walks the bytes in chunks.
async function fileBytesToBase64(file: File): Promise<string> {
  const buf = new Uint8Array(await file.arrayBuffer());
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < buf.length; i += chunk) {
    binary += String.fromCharCode(...buf.subarray(i, i + chunk));
  }
  return btoa(binary);
}

// formatFileSize renders a byte count the way the chosen-file list shows it.
export function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
