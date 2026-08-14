---
name: vision-tools
description: How to use Glance, Ground, Detect and Crop together — which one answers which question, why a comparison must be one call, when a description is the wrong return type, and how a located box feeds the next call. Read this before the second vision call of a task, and whenever an answer about an image came back vague, contradictory, or about the wrong thing.
---

# Seeing with four tools

You cannot see images. Four tools can, and they answer different questions:

| The question you are actually asking | The tool |
| --- | --- |
| "What does this show / say?" | `Glance` |
| "Where is X?" — one thing you can name | `Ground` |
| "Where are all the Xs?" — every instance of a kind | `Detect` |
| "Cut that box out as its own image" | `Crop` |

`Glance` answers *what*; `Ground` and `Detect` answer *where*; `Crop` makes
the next look closer. Only `Crop` costs nothing — the other three are a call
to a vision model and each one costs many times an ordinary sub-turn.

## Prose is the wrong answer to half the questions

Reach for `Ground` or `Detect` whenever the thing you need is a position, a
size, an alignment, or a count. "The heading looks slightly low" is not
something you can act on; `y1: 112` next to a sibling's `y1: 96` is. Asking
`Glance` for a measurement gets you a sentence about a measurement, which is
the same words whether the gap is 4px or 40px.

Ask `Glance` when the thing you need is what something *is*: what a page
shows, what an error message says, whether an element is present at all,
what two captures differ in qualitatively.

## Boxes are close, not exact

`Ground` and `Detect` return boxes in the original image's pixels, scaled
from a normalised grid — so the last pixel or few are not reliable. That is
accurate enough to crop with, to compare positions against, to say which of
two elements sits higher. It is not accurate enough to assert a padding value
or a border width from. When a number has to be exact, read it out of the CSS
or the DOM, not out of a box.

The coordinates are in the ORIGINAL image's pixels even when the file was
downscaled to fit the size limit — but the vision model's ability to *see* a
small target was not. When a result says an image was downscaled and the
locate then found nothing, capture the element on its own rather than
concluding it is absent.

## A box is a handle, not just an answer

The reason to locate something is usually to look at it properly:

1. `Screenshot` the page.
2. `Ground` it for the element you care about — get a box.
3. `Crop` that box to its own file, `scale` it up if it is small.
4. `Glance` the crop, where the element fills the frame instead of being
   forty pixels of a whole page.

`Glance` also takes a `region` directly, which sends only that part of the
image — the same win in one call when you do not need the crop as a file.

## Compare in one call, never two

To compare two images, pass both paths to a single `Glance` call. Two
separate calls cannot see each other's image, so comparing their answers
afterwards is comparing two independent guesses — two surfaces for error,
not a comparison. Images arrive labelled `Image 1`, `Image 2` in the order
you list them, and the first is read at higher resolution than the rest, so
put the one the question is really about first.

## When several boxes come back

If `Ground` returns a numbered list where you expected one box, your
description matched more than one element. Narrow it with what distinguishes
the one you mean — its text, its position, the block it sits in — rather than
guessing which of the boxes was meant. `Detect` is the tool for deliberately
enumerating every element of a kind.

## Ask for what you can act on

- Name the standard you are judging against. A vision model given no spec
  falls back on general web convention and reports deliberate choices as
  breakage.
- Put the data first and the question last: describe the images, paste the
  spec or the CSS, then ask.
- Be brief and concrete. Naming what you want examined beats instructing the
  model how to think — step-by-step scaffolding makes it over-analyse.
- Capture the element, not the page, when the question is about one control.
  A full-page capture is downscaled until small detail is unreadable, and no
  amount of prompting recovers pixels that were not sent.

## What these tools do not do

There is no OCR-a-tall-scrolling-page workflow, no pixel diff, and no
vectoriser here. For a very tall capture, screenshot the regions separately
rather than sending one image that will be downscaled past readability. To
find what changed between two renders, compare the markup or the CSS — a
one-word or few-pixel change is a rounding error to a vision model, and
asking it to spot one invites a confident wrong answer.

---

Adapted from the `vision-tools` skill in
[Anionex/agent-vision-toolkit](https://github.com/Anionex/agent-vision-toolkit)
(MIT, © 2026 Anionex — see `LICENSE.upstream`), whose CLIs this harness
reimplemented as the four tools above. The judgment is theirs; the tool names,
the call shapes, and the caveats about this harness's own limits are not.
