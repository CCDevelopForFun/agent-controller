# Architecture

Source diagrams and architecture references for Agent Controller. The top-level project [README](../../README.md) embeds [`architecture.svg`](architecture.svg) as the canonical pitch diagram; this folder holds that diagram's source and additional, more detailed views.

## Contents

| File | Format | Purpose |
|---|---|---|
| [`architecture.svg`](architecture.svg) | SVG | **Canonical diagram embedded in the main README.** Hand-authored — *not* exported from the `.excalidraw` (see the note below). The title is converted to vector paths so it renders identically regardless of installed fonts. |
| [`architecture.excalidraw`](architecture.excalidraw) | Excalidraw JSON | Editable whiteboard version of the same layout, kept coordinate-aligned with the SVG. Open with the VS Code Excalidraw extension or drag-and-drop into [excalidraw.com](https://excalidraw.com). |
| [`overview.md`](overview.md) | Mermaid | Detailed multi-adapter component diagram + layer/governance breakdown. |
| [`01-architecture.excalidraw`](01-architecture.excalidraw) | Excalidraw JSON | Original hand-drawn version generated during brainstorming. **Historical — intentionally frozen** at the MVP slice (single Pi adapter, registry loader unbuilt). Don't update it for new adapters. |

### The SVG and the Excalidraw are siblings, not source-and-output

`architecture.svg` contains no Excalidraw payload — it is hand-written SVG with
its own `<defs>`, filters, and font stack. Exporting `architecture.excalidraw`
over it would discard that styling (drop shadows, the vectorised title, the
dashed amber registry connector).

They do share one coordinate system, so a change to one is mechanical to mirror
in the other: the container is `x=112 y=305 w=633`, adapter boxes sit at `y=322`
with `h=74`, and the Excalidraw text elements are offset `+10 x` / `+26 y` from
their rectangle. **Update both in the same commit** and re-render the SVG to
check it (`rsvg-convert -w 1400 architecture.svg -o /tmp/check.png`) — text
overflow and connectors crossing labels are not visible in the markup.

## Conventions

- SVG for the polished diagram embedded in the main README; convert display text to vector paths so it renders without font dependencies.
- Mermaid (`.md` or `.mmd`) for detailed diagrams that should render inline on GitHub and stay in sync with the code.
- Excalidraw (`.excalidraw`) for editable sources and whiteboard-style sketches.
- Number files (`01-`, `02-`, ...) when there's a meaningful ordering across views.
- Each diagram file should be standalone (no cross-file `include` directives) so the GitHub renderer works without tooling.
