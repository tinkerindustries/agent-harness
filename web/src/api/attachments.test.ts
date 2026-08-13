import { describe, expect, it } from "vitest";
import {
  attachmentCapsFromSettings,
  formatFileSize,
  readAttachmentFiles,
  type AttachmentCaps,
} from "./attachments";
import type { SettingEntry } from "./settings";

// entry builds one GET /api/settings row with the fields this module reads.
function entry(key: string, def: string, value?: string): SettingEntry {
  return {
    key,
    group: "tools",
    type: "integer",
    default: def,
    description: "",
    secret: false,
    restart: false,
    set: value !== undefined,
    override: value !== undefined,
    ...(value !== undefined ? { value } : {}),
  };
}

function pngFile(name: string, size: number): File {
  return new File([new Uint8Array(size)], name, { type: "image/png" });
}

// The caps are read from GET /api/settings, never hardcoded: the effective
// value is the override when set, else the registry default.
describe("attachmentCapsFromSettings", () => {
  it("uses the override when the key is set, else the default", () => {
    const caps = attachmentCapsFromSettings([
      entry("tools.attachments_max_count", "8", "3"),
      entry("tools.attachments_max_bytes", "5242880"),
    ]);
    expect(caps).toEqual({ maxCount: 3, maxBytes: 5_242_880 });
  });
});

describe("readAttachmentFiles", () => {
  const caps: AttachmentCaps = { maxCount: 4, maxBytes: 5_242_880 };

  it("converts a valid selection into the wire payload, base64 included", async () => {
    const file = new File([new Uint8Array([1, 2, 3, 4])], "mockup.png", { type: "image/png" });
    const result = await readAttachmentFiles([file], caps);
    if (!result.ok) throw new Error(`expected success, got: ${result.error}`);
    expect(result.chosen).toHaveLength(1);
    expect(result.chosen[0].size).toBe(4);
    expect(result.chosen[0].attachment).toEqual({
      name: "mockup.png",
      mime_type: "image/png",
      data: "AQIDBA==", // base64 of [1,2,3,4]
    });
  });

  it("maps .jpg and .jpeg to image/jpeg and .webp to image/webp", async () => {
    const result = await readAttachmentFiles(
      [new File([new Uint8Array(1)], "a.jpg"), new File([new Uint8Array(1)], "b.JPEG"), new File([new Uint8Array(1)], "c.webp")],
      caps,
    );
    if (!result.ok) throw new Error(`expected success, got: ${result.error}`);
    expect(result.chosen.map((c) => c.attachment.mime_type)).toEqual(["image/jpeg", "image/jpeg", "image/webp"]);
  });

  it("rejects one file over the byte cap, naming the actual limit", async () => {
    const result = await readAttachmentFiles([pngFile("huge.png", caps.maxBytes + 1)], caps);
    if (result.ok) throw new Error("expected a rejection");
    expect(result.error).toContain("huge.png");
    expect(result.error).toContain(`over the ${caps.maxBytes}-byte per-file limit`);
  });

  it("rejects more files than the count cap, naming the actual limit", async () => {
    const many = Array.from({ length: caps.maxCount + 1 }, (_, i) => pngFile(`shot-${i}.png`, 10));
    const result = await readAttachmentFiles(many, caps);
    if (result.ok) throw new Error("expected a rejection");
    expect(result.error).toContain(`At most ${caps.maxCount} attachments are accepted, got ${many.length}`);
  });

  it("rejects a file outside the PNG/JPEG/WebP allowlist", async () => {
    const result = await readAttachmentFiles([new File([new Uint8Array(1)], "notes.txt")], caps);
    if (result.ok) throw new Error("expected a rejection");
    expect(result.error).toContain("not a PNG, JPEG, or WebP file");
  });

  it("returns an empty selection untouched", async () => {
    const result = await readAttachmentFiles([], caps);
    if (!result.ok) throw new Error(`expected success, got: ${result.error}`);
    expect(result.chosen).toEqual([]);
  });
});

describe("formatFileSize", () => {
  it("renders bytes, KB, and MB", () => {
    expect(formatFileSize(512)).toBe("512 B");
    expect(formatFileSize(2048)).toBe("2.0 KB");
    expect(formatFileSize(3 * 1024 * 1024)).toBe("3.0 MB");
  });
});
