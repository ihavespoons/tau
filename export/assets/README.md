# Export assets

These files are embedded into the tau binary and substituted into a single
self-contained HTML page by the `export` package. Nothing here is fetched at
runtime: an exported session opens from `file://` with no network access.

| File | Origin | Licence |
|---|---|---|
| `template.html` | Pi v0.82.1, `packages/coding-agent/src/core/export-html/template.html` | MIT © Mario Zechner |
| `template.css` | Pi v0.82.1, same directory | MIT © Mario Zechner |
| `template.js` | Pi v0.82.1, same directory | MIT © Mario Zechner |
| `vendor/marked.min.js` | [marked](https://github.com/markedjs/marked) v18.0.5 | MIT © MarkedJS |
| `vendor/highlight.min.js` | [highlight.js](https://github.com/highlightjs/highlight.js) v11.9.0 | BSD-3-Clause |

Full licence texts are in `THIRD-PARTY-NOTICES.md` at the repository root.

## Divergences from Pi

The viewer is otherwise byte-identical to Pi's. Three identifiers were renamed
so a tau export and a Pi export can coexist in one browser without sharing
state:

| Pi | tau | Where |
|---|---|---|
| `meta[name="pi-url-params"]` | `meta[name="tau-url-params"]` | `template.js` |
| `meta[name="pi-share-base-url"]` | `meta[name="tau-share-base-url"]` | `template.js` |
| `pi-share:v1:sidebar-width` | `tau-share:v1:sidebar-width` | `template.js` (localStorage key) |

The two `meta` tags are read only when the page is embedded in a host that
injects them via `srcdoc`; opening the file directly falls back to
`window.location`, so a standalone export is unaffected either way.

## Template placeholders

`template.html` carries `{{CSS}}`, `{{JS}}`, `{{SESSION_DATA}}`, `{{MARKED_JS}}`
and `{{HIGHLIGHT_JS}}`. `template.css` carries `{{THEME_VARS}}`, `{{BODY_BG}}`,
`{{CONTAINER_BG}}` and `{{INFO_BG}}` inside its first `:root` block. Each is
substituted exactly once, in that order. Re-vendoring a newer Pi means keeping
those markers intact.
