# Architecture

Source diagrams and architecture references for Agent Controller. The top-level project [README](../../README.md) embeds [`architecture.svg`](architecture.svg) as the canonical pitch diagram; this folder holds that diagram's source and additional, more detailed views.

## Contents

| File | Format | Purpose |
|---|---|---|
| [`architecture.svg`](architecture.svg) | SVG | **Canonical diagram embedded in the main README.** The title is converted to vector paths so it renders identically regardless of installed fonts. |
| [`architecture.excalidraw`](architecture.excalidraw) | Excalidraw JSON | Editable source for `architecture.svg`. Open with the VS Code Excalidraw extension or drag-and-drop into [excalidraw.com](https://excalidraw.com). |
| [`overview.md`](overview.md) | Mermaid | Detailed dual-adapter component diagram + layer/governance breakdown. |
| [`01-architecture.excalidraw`](01-architecture.excalidraw) | Excalidraw JSON | Original hand-drawn version generated during brainstorming. |

## Conventions

- SVG for the polished diagram embedded in the main README; convert display text to vector paths so it renders without font dependencies.
- Mermaid (`.md` or `.mmd`) for detailed diagrams that should render inline on GitHub and stay in sync with the code.
- Excalidraw (`.excalidraw`) for editable sources and whiteboard-style sketches.
- Number files (`01-`, `02-`, ...) when there's a meaningful ordering across views.
- Each diagram file should be standalone (no cross-file `include` directives) so the GitHub renderer works without tooling.
