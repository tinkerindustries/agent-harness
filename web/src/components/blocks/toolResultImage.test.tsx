import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { ToolCallPayload, ToolResultPayload } from "../../api/types";
import { InlineImage } from "./InlineImage";
import { ToolResultBlock } from "./ToolResultBlock";
import { GALLERY_CLS, TILE_CLS } from "./ScreenshotGallery";

// The inline image a tool result carries: a data URI (image_url) is rendered
// as a bounded tile whose full-size view is a new tab, and a tool result
// without one renders no image at all. There is no DOM harness in this repo
// (TESTING.md), so the render is checked through react-dom/server's static
// markup — the components are pure enough to render to a string, which is
// exactly the property the frozen-block design relies on.

const DATA_URI = "data:image/png;base64,iVBORw0KGgo=";
// What the HTTP surface actually sends in place of those bytes
// (internal/httpapi/eventimage.go).
const HREF = "/api/sessions/sess-1/events/5/image";

// ToolResultBlockShape is the block ToolResultBlock renders, mirroring
// web/src/api/fold.ts's tool_result Block variant.
type ToolResultBlockShape = { type: "tool_result"; seq: number; call?: ToolCallPayload } & ToolResultPayload;

function readBlock(overrides: Partial<ToolResultPayload> = {}): ToolResultBlockShape {
  return {
    type: "tool_result",
    seq: 5,
    tool_call_id: "call_r",
    name: "Read",
    content: "Image: shot.png",
    ...overrides,
  };
}

describe("InlineImage", () => {
  it("renders the data URI as an image inside a bounded tile", () => {
    const html = renderToStaticMarkup(<InlineImage url={DATA_URI} />);

    expect(html).toContain(`src="${DATA_URI}"`);
    // The tile classes are the gallery's own, so a data URI gets the same
    // size bounds a workspace screenshot gets.
    expect(html).toContain(`class="${GALLERY_CLS}"`);
    expect(html).toContain(`class="${TILE_CLS}"`);
  });

  it("opens the full-size view in a new tab, like the screenshot gallery", () => {
    const html = renderToStaticMarkup(<InlineImage url={DATA_URI} />);

    expect(html).toContain('target="_blank"');
    expect(html).toContain(`href="${DATA_URI}"`);
  });
});

describe("ToolResultBlock with an inline image", () => {
  it("renders the image when the tool result carries image_url, leading the body", () => {
    const html = renderToStaticMarkup(<ToolResultBlock block={readBlock({ image_url: DATA_URI })} />);

    expect(html).toContain(`src="${DATA_URI}"`);
    // The image leads the text, the way a Screenshot result's images lead.
    expect(html.indexOf(`src="${DATA_URI}"`)).toBeLessThan(html.indexOf("Image: shot.png"));
  });

  // What a transcript loaded over HTTP actually gets: the bytes are served
  // separately and the payload carries a URL onto them.
  it("renders the image when the tool result carries image_href", () => {
    const html = renderToStaticMarkup(<ToolResultBlock block={readBlock({ image_href: HREF })} />);

    expect(html).toContain(`src="${HREF}"`);
    expect(html.indexOf(`src="${HREF}"`)).toBeLessThan(html.indexOf("Image: shot.png"));
    // Fetched, so it may be deferred until the tile is on screen — the whole
    // point of not carrying it in the transcript.
    expect(html).toContain('loading="lazy"');
  });

  // A payload that somehow carries both is a served URL plus bytes nobody
  // needed. Preferring the href keeps the page from decoding megabytes it
  // was not going to display.
  it("prefers image_href over image_url when both are present", () => {
    const html = renderToStaticMarkup(<ToolResultBlock block={readBlock({ image_href: HREF, image_url: DATA_URI })} />);

    expect(html).toContain(`src="${HREF}"`);
    expect(html).not.toContain(DATA_URI);
  });

  it("renders no image when the tool result carries neither", () => {
    const html = renderToStaticMarkup(<ToolResultBlock block={readBlock()} />);

    expect(html).not.toContain("<img");
    // The text still renders.
    expect(html).toContain("Image: shot.png");
  });
});
