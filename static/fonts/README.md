# Vendored typefaces — charm-web (ADR-0018)

The charm-web design language uses two monospace families, both licensed under the
[SIL Open Font License 1.1](https://openfontlicense.org/) and therefore freely
redistributable in this repo:

| Family | Role | Weights needed | Expected files |
|---|---|---|---|
| [JetBrains Mono](https://github.com/JetBrains/JetBrainsMono) | body / UI / code | 400, 500, 700 | `jetbrains-mono-400.woff2`, `jetbrains-mono-500.woff2`, `jetbrains-mono-700.woff2` |
| [Space Mono](https://github.com/googlefonts/spacemono) | display / wordmark | 400, 700 | `space-mono-400.woff2`, `space-mono-700.woff2` |

`static/tokens.css` declares the `@font-face` rules pointing at the files above; the
binary embeds whatever is present in this directory (`//go:embed static` in `assets.go`)
and serves it same-origin — **no font CDN, ever** (ADR-0018, CSP `default-src 'self'`).
Until the woff2 files are vendored the UI falls back to the system `ui-monospace` stack;
`fonts_test.go` skips (loudly) instead of failing when a file is absent, and asserts
glyph coverage (arrows + core ASCII + the box-drawing/status glyphs the UI renders) the
moment it lands.

## Vendoring the files

From a machine with network access:

1. Download the official releases (or use google-webfonts-helper) for the weights above.
2. Convert TTF → WOFF2 if needed (`woff2_compress` or `fonttools ttLib.woff2`).
   Keep the full Latin set plus the glyphs `fonts_test.go` asserts: `← → ✓ ✗ · ❯ ●`.
3. Drop the files here with the exact names in the table.
4. Copy each family's `OFL.txt` alongside as `OFL-jetbrains-mono.txt` / `OFL-space-mono.txt`.
5. `go test ./...` — `fonts_test.go` and `assets_test.go` verify the files.

Removed families (retired by ADR-0018): Zilla Slab, IBM Plex Sans, IBM Plex Mono.
