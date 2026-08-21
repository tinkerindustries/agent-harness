import { GALLERY_CLS, TILE_CLS } from "./ScreenshotGallery";

// InlineImage renders the image a tool result carries: either the bytes
// themselves as a data URI (ToolResultPayload.ImageURL,
// docs/KIMI-INTEGRATION.md §4.5) or, as the HTTP surface actually sends it,
// a URL onto GET /api/sessions/{id}/events/{seq}/image that decodes them out
// of the event on demand (internal/httpapi/eventimage.go). Both are an
// <img src> and the caller picks (ToolResultBlock.tsx ToolResultBody).
//
// It sits beside ScreenshotGallery rather than merging with it, because the
// two address different things and fail differently. The gallery names
// workspace files, which get cleaned up, so it carries a "no longer
// available" state. These bytes are in the event log, which is append-only:
// the picture at a given seq is the same picture forever, and the response
// serving it says so with an immutable Cache-Control, so there is no missing
// case to render and no reason to fetch it twice.
//
// The tile deliberately reuses the gallery's own classes, so this image gets
// exactly the same bounds a workspace screenshot gets — capped at the tile
// width and 380px tall, never stretched — and the full-size view is the same
// interaction: a new tab rather than a lightbox.
//
// The image is loaded lazily, which is the point of serving it over HTTP at
// all: a transcript with thirty renders in it fetches the ones somebody
// scrolls to, in parallel, rather than carrying all thirty before the first
// line of text can be read.
export function InlineImage({ url }: { url: string }) {
  return (
    <div className={GALLERY_CLS}>
      <figure className={TILE_CLS}>
        <a href={url} target="_blank" rel="noreferrer" onClick={openFullSize}>
          <img
            className="block w-full max-h-[380px] object-contain object-top border border-border rounded bg-background"
            src={url}
            loading="lazy"
            decoding="async"
            alt="the image the model read"
          />
        </a>
      </figure>
    </div>
  );
}

// openFullSize opens the image at its real size in a new tab.
//
// An ordinary URL needs none of this: the click is left alone and the anchor
// navigates. The blob dance below is only for a data URI, which Chromium
// refuses to navigate a top frame to ("Not allowed to navigate top frame to
// data URL") — the bytes are fetched (fetch handles data: URLs), wrapped in
// a blob URL, and opened; the href stays the data URI so a browser that does
// allow top-frame data navigation still works. The blob URL is released
// after a grace period — revoking immediately would abort the new tab's
// load, and once it has loaded the tab holds the image itself.
async function openFullSize(e: React.MouseEvent<HTMLAnchorElement>) {
  // currentTarget.href is resolved and absolute, so a served URL never looks
  // like a data one however it was written.
  if (!e.currentTarget.href.startsWith("data:")) return;
  e.preventDefault();
  try {
    const blob = await (await fetch(e.currentTarget.href)).blob();
    const objectUrl = URL.createObjectURL(blob);
    window.open(objectUrl, "_blank", "noopener");
    setTimeout(() => URL.revokeObjectURL(objectUrl), 60_000);
  } catch {
    // Leave the click unhandled: the plain data: href is the fallback.
  }
}
