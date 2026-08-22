---
name: blender-harness-notes
description: Read this before calling any mcp__blender__ tool in this harness. The twenty-six Blender tools reach two different Blenders — fourteen drive the operator's live interactive session over TCP, and the twelve named *_for_cli shell out to blender --background inside this container — and almost every confusing failure is a call aimed at the wrong half. Also covers the two screenshot tools that fail unless size_limit_in_bytes is set, which filesystem each half resolves a path on, and where the bundled Blender API and manual docs are.
allowed-tools:
  - Bash
---

# Blender in this harness

The Blender MCP server already told you how Blender itself behaves — the
datablock model, active object versus selection, the depsgraph update, the
bmesh flush — in its own instructions at the top of this session. **This file
does not repeat any of that.** It is only the part the server cannot know:
which Blender it is talking to from in here.

## The twenty-six tools reach two different Blenders

Get this wrong and the error will name Python, or a socket, and neither will
mention the thing that is actually wrong.

**Fourteen tools drive the operator's live Blender** — the one with a scene
open on a screen somewhere. `execute_blender_code`, the `jump_to_*`
navigation, the screenshots. They talk over TCP to an add-on running inside
that Blender.

**Twelve tools do not.** Every name ending `_for_cli` shells out to
`blender --background` from the MCP server subprocess, which runs *inside this
container*. They open a `.blend` in a throwaway process. No network, no
add-on, no operator's scene.

So `execute_blender_code` and `execute_blender_code_for_cli` are not the same
tool with a convenience flag. One mutates a scene a person is looking at; the
other opens a file, runs, and throws the process away.

## When the add-on half fails

A connection error naming the MCP server means the subprocess dialled and got
nothing. That is as far as it got — it is not a Blender error, and it does not
mean Blender is broken.

The dial goes from inside this container, so a Blender running only on the
operator's own machine is a `localhost` the container cannot reach. Whether it
works at all is the operator's configuration, not something you can fix from a
session. If the add-on half is unreachable, say so and use the `_for_cli` half
if the task allows it — do not retry the same call hoping for a different
answer.

The CLI half needs no network. It needs the Blender binary, which this image
carries for exactly these twelve tools.

## Two tools that fail unless you pass an argument

`get_screenshot_of_window_as_image` and `get_screenshot_of_area_as_image`
**must** be given `size_limit_in_bytes`. Left at the default of `0` — meaning
no limit — a normal-sized Blender window fails with:

    Invalid response from Blender at …:9876: Unterminated string

A full-resolution PNG, base64'd, overruns the framing of the add-on's TCP
bridge and the server is left parsing a truncated string. Any non-zero value
works; anything from 8000 to 300000 bytes has been measured fine. This is the
vendored server's own wire format and cannot be fixed from this side, so pass
the argument every time.

`get_screenshot_of_window_as_json` has no such problem — that payload is
small. The other twenty-four tools work with their defaults.

## Paths resolve on different filesystems

The add-on resolves a path on the **host**. The `_for_cli` tools resolve one
inside **this container**. They agree only under the workspace root, which is
mounted at the same absolute path on both sides on purpose.

So write anything you intend to read back — a `render_viewport_to_path`
output, say — under the workspace root. A path anywhere else lands on
whichever filesystem that half happened to be standing on, and the other half
will not find it.

## The bundled documentation

The server ships plain-text RST beside itself: `data/api/` is the Blender
Python API reference, `data/manual/` is the user manual. Search them with
`grep` and read them with the ordinary file tools. Consult `data/api/` before
writing `bpy` code rather than recalling an operator signature — enum values
and property names are exactly the kind of thing worth reading rather than
remembering.
