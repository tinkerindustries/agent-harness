// Attachment handling for the two places a person puts an image into a
// session — the start form's file input and the chat composer's paste — split
// out of the components so it can be unit-tested without a DOM
// (web/CLAUDE.md: components are not unit-tested): the file-to-payload
// conversion (a File into the {name, mime_type, data} shape POST /api/runs,
// /steer and /resume all accept) and the cap validation, with the limits read
// from GET /api/settings rather than hardcoded, so an operator who changes
// tools.attachments_max_count or tools.attachments_max_bytes sees both
// surfaces change with it.
//
// The two entry points differ only in where the name comes from. A file
// chosen from disk has one worth keeping; a pasted screenshot does not, and
// pastedImageName below is the whole reason this module needed a second
// function rather than a flag.

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

// pastedImageName is the file name a pasted image is stored and materialised
// under. It exists because the clipboard has no useful one: Chrome hands
// every screenshot over as "image.png", so two pastes into the same session
// would be two attachments fighting over one path in
// scratch/attachments/ — the second overwriting the first, and the model
// reading the wrong picture for a name it was told about.
//
// stamp is a caller-supplied compact timestamp (see pasteStamp) and index
// disambiguates a multi-image paste within it, so a name is unique across
// pastes, across messages, and within one clipboard's worth of files without
// this module ever reading the clock itself — which is what keeps it
// testable.
export function pastedImageName(mime: string, stamp: string, index: number): string {
  const ext = pastedExtensions[mime] ?? ".png";
  return `pasted-${stamp}-${index + 1}${ext}`;
}

// The reverse of attachmentMIMEByExtension: a pasted image is named from its
// type, since its own name is thrown away. JPEG resolves to ".jpg" — either
// spelling is accepted on the way back in, and one of them has to be the one
// we write. It doubles as the paste allowlist, so a clipboard carrying an SVG
// or a GIF stages nothing rather than staging something the server refuses.
const pastedExtensions: Record<string, string> = {
  "image/png": ".png",
  "image/jpeg": ".jpg",
  "image/webp": ".webp",
};

// pasteStamp renders a Date as the compact, sortable stamp pastedImageName
// builds on: YYYYMMDD-HHMMSS-mmm, local time, milliseconds included so two
// pastes in the same second still land on different names.
export function pasteStamp(at: Date): string {
  const p = (n: number, width = 2) => String(n).padStart(width, "0");
  return (
    `${at.getFullYear()}${p(at.getMonth() + 1)}${p(at.getDate())}-` +
    `${p(at.getHours())}${p(at.getMinutes())}${p(at.getSeconds())}-${p(at.getMilliseconds(), 3)}`
  );
}

// readPastedImages turns the image files off a paste into the same wire
// payload readAttachmentFiles produces, applying the same caps against the
// whole staged set rather than against this paste alone — the caps are what
// the endpoint enforces on the message, and a second paste that pushes the
// total over the count limit has to be refused here rather than at send.
//
// Every image is renamed, including one that arrived with a name of its own:
// the clipboard's names are not merely useless but actively harmful — the
// commonest of them, "image.png", is the same for every screenshot anybody
// ever pastes, so honouring it would have the second paste into a session
// overwrite the first in scratch/attachments/ while the model was told about
// both, and the earlier message's gallery would quietly start showing the
// later picture. One minted name per image closes that off for the price of
// a dragged mockup.png reading as pasted-<stamp>-1.png in the transcript,
// which is a name a person can still find on disk.
//
// Non-image files in the paste are ignored rather than refused: a paste is
// usually text, and text that happens to arrive alongside a file is the
// composer's business, not an error.
export async function readPastedImages(
  files: File[],
  caps: AttachmentCaps,
  staged: ChosenAttachment[],
  stamp: string,
): Promise<{ ok: true; chosen: ChosenAttachment[] } | { ok: false; error: string }> {
  const images = files.filter((f) => f.type in pastedExtensions);
  if (images.length === 0) return { ok: true, chosen: [] };
  if (staged.length + images.length > caps.maxCount) {
    return {
      ok: false,
      error: `At most ${caps.maxCount} images can be attached to one message; this paste would make ${staged.length + images.length}.`,
    };
  }
  const chosen: ChosenAttachment[] = [];
  for (const [i, file] of images.entries()) {
    if (file.size > caps.maxBytes) {
      return {
        ok: false,
        error: `A pasted image is ${formatFileSize(file.size)}, over the ${formatFileSize(caps.maxBytes)} per-image limit.`,
      };
    }
    const name = pastedImageName(file.type, stamp, i);
    chosen.push({ attachment: { name, mime_type: file.type, data: await fileBytesToBase64(file) }, size: file.size });
  }
  return { ok: true, chosen };
}

// formatFileSize renders a byte count the way the chosen-file list shows it.
export function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
