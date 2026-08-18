# Diagram sources

| File | What it is |
|---|---|
| `architecture.mmd` | Mermaid source. `make diagram` renders it to `architecture.png` |
| `architecture.png` | What the README embeds. Regenerate rather than edit |
| `architecture.drawio` | The same diagram as an editable draw.io file, for anyone who would rather work in a canvas |

The PNG is generated from the mermaid source, so that is the one to change if you want the
README picture to change. The draw.io file is kept in step by hand; if you edit it, export a
PNG over `architecture.png` and leave the mermaid alone.
