import { useState } from "react";
import { useSessionId } from "../../hooks";
import { screenshotUrl, trimWorkspace } from "./toolArgs";
import { TOOL_DETAIL_CLS } from "./blockStyles";
import { cn } from "@/lib/utils";

// The tile bounds a workspace screenshot and a tool result's inline data-URI
// image (InlineImage.tsx) share, so a data URI gets exactly the same size
// caps a fetched screenshot gets — never stretched, capped at the tile width
// and 380px tall. Exported so InlineImage.tsx reuses the values rather than
// copying them, matching its own comment ("deliberately reuses the
// gallery's own classes").
export const GALLERY_CLS = "flex flex-wrap gap-3 my-1.5";
export const TILE_CLS = "m-0 min-w-0 flex-[0_1_320px]";

// ScreenshotGallery renders the images a Screenshot, Glance, Ground, Detect
// or Crop call names, read from the session's live workspace through
// GET /api/sessions/{id}/screenshot (docs/TOOLS.md, "Seeing the
// screenshots"). Without it a transcript reports what the vision model said
// about a page and never shows the page, which leaves the one artefact that
// would settle whether the model was right out of the record.
//
// The images are the session's own workspace files, so they are gone once
// that workspace is cleaned up. A load failure is therefore an ordinary
// state, not an error: the tile keeps the file name and says the image is no
// longer available, because a path an operator can go and look for is more
// use than a broken-image glyph.
export function ScreenshotGallery({ paths }: { paths: string[] }) {
  const sessionId = useSessionId();
  if (paths.length === 0) return null;

  // No session id means no addressable image (the perf harnesses, which
  // render blocks with no session behind them). The paths are still worth
  // showing — they are what the call was made with.
  if (sessionId === "") {
    return (
      <ul className="my-1.5 pl-[18px] text-sm">
        {paths.map((path) => (
          <li key={path}>
            <code className={TOOL_DETAIL_CLS}>{trimWorkspace(path)}</code>
          </li>
        ))}
      </ul>
    );
  }

  return (
    <div className={GALLERY_CLS}>
      {paths.map((path) => (
        <ScreenshotTile key={path} sessionId={sessionId} path={path} />
      ))}
    </div>
  );
}

function ScreenshotTile({ sessionId, path }: { sessionId: string; path: string }) {
  const [failed, setFailed] = useState(false);
  const label = trimWorkspace(path);
  const url = screenshotUrl(sessionId, path);

  if (failed) {
    return (
      <figure className={cn(TILE_CLS, "screenshot-missing")}>
        <div className="flex h-24 items-center justify-center px-3 text-center text-sm text-muted-foreground border border-dashed border-border rounded">
          screenshot no longer available
        </div>
        <figcaption className="mt-1 text-xs wrap-anywhere">
          <code className={TOOL_DETAIL_CLS}>{label}</code>
        </figcaption>
      </figure>
    );
  }

  return (
    <figure className={TILE_CLS}>
      {/* The full-size image opens in a tab rather than a lightbox: a
          screenshot is worth looking at at its real size, and the browser
          already has a good viewer for one image. */}
      <a href={url} target="_blank" rel="noreferrer">
        <img
          className="block w-full max-h-[380px] object-contain object-top border border-border rounded bg-background"
          src={url}
          alt={label}
          loading="lazy"
          onError={() => setFailed(true)}
        />
      </a>
      <figcaption className="mt-1 text-xs wrap-anywhere">
        <code className={TOOL_DETAIL_CLS}>{label}</code>
      </figcaption>
    </figure>
  );
}
