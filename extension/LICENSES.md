# Third-party licenses

## Neovim mark

- **Work:** the Neovim mark (the interlocking "N"), as published at
  <https://github.com/neovim/neovim.github.io/blob/master/static/logos/neovim-mark-flat.svg>
- **Author:** Jason Long
- **License:** Creative Commons Attribution 3.0 Unported (CC BY 3.0)
- **License text:** <https://creativecommons.org/licenses/by/3.0/> ·
  legal code: <https://creativecommons.org/licenses/by/3.0/legalcode>

### Statement of modification

CC BY 3.0 requires that changes be indicated. **This work has been modified.**
Two derivatives of the official `neovim-mark-flat.svg` are shipped here:

| File | Modification |
| --- | --- |
| `nvim-mark.svg` | **Recolored.** The two original brand fills (`#3C92D2`, `#57A143`) were both replaced with `currentColor` so the mark inherits the surrounding text colour and stays legible on light and dark themes. The Sketch-authoring metadata (`sketch:*` attributes, `<description>`, empty `<defs>`) was stripped and the path geometry left untouched. |
| `icons/16.png`, `icons/48.png`, `icons/128.png` | **Rasterized, padded and cropped.** Rendered from the unmodified two-colour `neovim-mark-flat.svg` and centred on a transparent square canvas at ~86% of the icon box. Colours unchanged; geometry unchanged. |

Neither derivative is endorsed by, nor implies endorsement from, Jason Long or
the Neovim project. "Neovim" is used here only to identify the editor this
extension talks to.

## codelink itself

The extension source in this directory (`background.js`, `content.js`,
`content.css`, `manifest.template.json`) carries whatever license the enclosing
repository carries; it is not derived from any third-party code.
