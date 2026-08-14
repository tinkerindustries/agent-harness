// InlineImage renders the image bytes a tool result already carries — the
// image_url data URI Read returned to a vision provider
// (docs/KIMI-INTEGRATION.md §4.5, ToolResultPayload.ImageURL). It is the
// inline counterpart of ScreenshotGallery: that component addresses workspace
// files by session id over HTTP, while these bytes are already in the event,
// so there is nothing to fetch, no session id to thread through, and no "no
// longer available" state — a data URI cannot go away.
//
// The tile deliberately reuses the gallery's own classes, so a data URI gets
// exactly the same bounds a workspace screenshot gets — capped at the tile
// width and 380px tall, never stretched — and the full-size view is the same
// interaction: a new tab rather than a lightbox. One difference is forced by
// the medium: Chromium refuses top-frame navigation to data: URLs ("Not
// allowed to navigate top frame to data URL"), so the new-tab open goes
// through a blob URL created from the bytes on click.
export function InlineImage({ url }: { url: string }) {
  return (
    <div className="screenshot-gallery">
      <figure className="screenshot-tile">
        <a href={url} target="_blank" rel="noreferrer" onClick={openFullSize}>
          <img src={url} alt="the image the model read" />
        </a>
      </figure>
    </div>
  );
}

// openFullSize opens the image at its real size in a new tab. The data URI
// cannot navigate a top frame in Chromium, so the bytes are fetched (fetch
// handles data: URLs), wrapped in a blob URL, and opened; the href stays the
// data URI so a browser that does allow top-frame data navigation still
// works. The blob URL is released after a grace period — revoking
// immediately would abort the new tab's load, and once it has loaded the tab
// holds the image itself.
async function openFullSize(e: React.MouseEvent<HTMLAnchorElement>) {
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
