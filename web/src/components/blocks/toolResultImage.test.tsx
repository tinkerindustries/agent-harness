import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { ToolCallPayload, ToolResultPayload } from "../../api/types";
import { InlineImage } from "./InlineImage";
import { ToolResultBlock } from "./ToolResultBlock";

// The inline image a tool result carries: a data URI (image_url) is rendered
// as a bounded tile whose full-size view is a new tab, and a tool result
// without one renders no image at all. There is no DOM harness in this repo
// (TESTING.md), so the render is checked through react-dom/server's static
// markup — the components are pure enough to render to a string, which is
// exactly the property the frozen-block design relies on.

const DATA_URI = "data:image/png;base64,iVBORw0KGgo=";

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
    expect(html).toContain('class="screenshot-gallery"');
    expect(html).toContain('class="screenshot-tile"');
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

  it("renders no image when the tool result carries no image_url", () => {
    const html = renderToStaticMarkup(<ToolResultBlock block={readBlock()} />);

    expect(html).not.toContain("<img");
    // The text still renders.
    expect(html).toContain("Image: shot.png");
  });
});
