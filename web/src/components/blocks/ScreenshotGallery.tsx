import { useState } from "react";
import { useSessionId } from "../../hooks";
import { screenshotUrl, trimWorkspace } from "./toolArgs";

// ScreenshotGallery renders the images a Screenshot or ReviewScreenshot call
// names, read from the session's live workspace through
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
      <ul className="screenshot-paths">
        {paths.map((path) => (
          <li key={path}>
            <code className="tool-detail">{trimWorkspace(path)}</code>
          </li>
        ))}
      </ul>
    );
  }

  return (
    <div className="screenshot-gallery">
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
      <figure className="screenshot-tile screenshot-missing">
        <div className="screenshot-missing-body">screenshot no longer available</div>
        <figcaption>
          <code className="tool-detail">{label}</code>
        </figcaption>
      </figure>
    );
  }

  return (
    <figure className="screenshot-tile">
      {/* The full-size image opens in a tab rather than a lightbox: a
          screenshot is worth looking at at its real size, and the browser
          already has a good viewer for one image. */}
      <a href={url} target="_blank" rel="noreferrer">
        <img src={url} alt={label} loading="lazy" onError={() => setFailed(true)} />
      </a>
      <figcaption>
        <code className="tool-detail">{label}</code>
      </figcaption>
    </figure>
  );
}
