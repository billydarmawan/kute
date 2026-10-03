# kute.dev — site design

How the marketing site is built and what its rules are. For the TUI's design
spec see [`README.md`](README.md) in this directory; this file is only about
the website.

The site's palette mirrors the TUI's semantic `Theme` so the HTML-rebuilt
terminal mockups look like the real product. The homepage hero and explorer
use paired dark/light `kute --demo` PNG captures instead: their job is proof,
not approximation, and they follow the website's selected theme. There is one
deliberate palette exception and it is called out below.

## Who it is for

**Every page is written for someone who wants to use kute**, not for someone
who wants to work on it. Architecture, design rationale and contributor
process live in the repo — this site explains what a user sees and does.

In practice that rules out a whole vocabulary: kind registry, command table,
semantic token struct, watch cache, skeleton, informer, `sh.helm.release.v1`,
"data not code", and any sentence whose subject is a Go type. Each of those
has a user-facing translation and the translation is what ships — "your CRDs
appear with the same tables and keys as built-in kinds, discovered on connect"
rather than "kinds are registry entries". Regeneration commands, `.tape`
files and repo paths are not published either; a visitor cannot run them.

The four pages map onto the journey — understand (`/`), learn (`/guide.html`),
try (`/install.html`), trust (`/verify.html`) — and each answers its question
once. The old `features.html` existed because the home page had become a
feature page; merging them is what the guide's existence made possible.

## How the pages are built

There are no hand-maintained HTML pages. `cmd/site` renders `website/dist/`
from:

| Source | What it holds |
| --- | --- |
| `website/pages/<slug>.html` | The body of one page, everything between the nav and the footer |
| `website/templates/page.html` | The shell: head, skip link, nav slot, `<main>`, footer, scripts |
| `website/templates/nav.html` | The nav, including `aria-current` on the active link |
| `website/templates/platform-install.html` | The platform-aware install panel shared by Home, Guide, and Verify |
| `website/templates/icons.html` | SVG fragments used more than once |
| `website/site.json` | Per-page metadata plus site-wide values |

Page bodies are parsed against a clone of the shared template set, so a body
can call `{{template "copyicon"}}` rather than pasting the same SVG path a
dozen times. Home, Guide, and Verify invoke the same platform-aware install
panel directly; Install already contains the full installation guide, and 404
does not need an install surface.

`website/dist/` is generated and gitignored. Build a complete local copy,
open it in a browser, and serve it until interrupted with:

```sh
scripts/run-website.sh
```

Pass a port as the first argument to override `4174`, or set
`KUTE_SITE_NO_OPEN=1` to skip opening a browser. Assets live at
`website/assets/`, while product media lives in `docs/assets/` — homepage
screenshots under `shots/`, guide clips under `demos/`, and the tapes they are
recorded from under `tapes/` (`docs/assets/README.md` documents that layout and
how to add a checkpoint). The script merges the two output directories into
`website/dist/assets/` just as the deploy job does, flattening them, which is
why a page reference is `/assets/<name>` and never names the subdirectory.

`text/template`, not `html/template`: there is no untrusted input, and
contextual escaping rewrites content inside attributes (an apostrophe in a
meta description becomes `&#39;`), which silently changes what is emitted.

## Colour tokens

Defined three times in `website/assets/styles.css` — once for dark, then the
light palette for an explicit `data-theme="light"` and again under
`prefers-color-scheme`. Plain CSS cannot share a declaration block between a
selector and an at-rule. The two light copies are asserted identical by
`TestLightPaletteBlocksMatch`, so they cannot drift.

| Token | Dark | Light |
| --- | --- | --- |
| `--bg` | `#0b0b10` | `#f7f7fa` |
| `--bg-chrome` | `#0e0e15` | `#eef0f4` |
| `--bg-strip` | `#0c0c12` | `#f2f3f7` |
| `--bg-card` | `#111119` | `#ffffff` |
| `--bg-card-hi` | `#16121e` | `#f4f1fa` |
| `--bg-palette` | `#101018` | `#ffffff` |
| `--border` | `#26263a` | `#d5d7e0` |
| `--border-subtle` | `#1c1c2c` | `#e4e6ee` |
| `--text` | `#f0f0fa` | `#14141c` |
| `--text-primary` | `#d8d8e8` | `#2a2a38` |
| `--text-secondary` | `#9a9ab2` | `#565668` |
| `--text-dim` | `#8f8fa4` | `#5b5b6e` |
| `--text-faint` | `#83839f` | `#64647d` |
| `--text-ghost` | `#78789b` | `#6d6d90` |
| `--accent` | `#a78bfa` | `#6b46d9` |
| `--accent-hi` | `#c4b5fd` | `#5936b8` |
| `--sel-bg` | `#1d1633` | `#ece5fb` |
| `--good` | `#34d17b` | `#117844` |
| `--warn` | `#e8c74a` | `#866208` |
| `--bad` | `#ef6a6a` | `#bc3131` |
| `--bad-soft` | `#ef8a8a` | `#c52e2e` |
| `--bad-text` | `#f0b7b7` | `#a02c2c` |
| `--info` | `#6aa8ef` | `#2767bf` |

**The quiet text tiers deliberately do not match the TUI.** `--text-dim`,
`--text-faint` and `--text-ghost` are stronger here than in the terminal.
They carry real prose on this page and have to clear WCAG AA against `--bg`;
a terminal running under the user's own colour scheme is not held to that
standard (the TUI's `TextDim` now clears AA too, but its `TextFaint` and
`TextGhost` don't, and syncing `--text-dim` alone would invert the ramp). Before the change `--text-ghost`
was at 2.08:1. `TestTextContrastMeetsAA` fails any token below 4.5:1, so
re-syncing them to the TUI palette will not pass CI.

Other tokens: `--radius-card: 14px`, `--radius-pill: 9999px`,
`--radius-term: 10px`, `--max-width: 1180px`, `--section-gap: 96px`, and a
four-step shadow scale. Fonts are Sora (display), Inter (UI) and JetBrains
Mono (terminal mockups), linked from the page head — never `@import`, which is
undiscoverable until the stylesheet has parsed and serialises the whole
third-party round trip behind it. Only weights actually used are requested.

## Rules

- **Both themes, always.** Anything new must work in dark and light, including
  under `prefers-color-scheme` with the toggle untouched. A selector list
  ending in a comma before an `@media` does not parse and silently drops the
  whole rule — that bug left the light theme with a dark nav bar for a while.
- **Every internal link is root-absolute.** GitHub Pages serves `404.html` for
  any missing path, so a relative asset path on a URL like `/a/b/c` resolves
  against `/a/b/` and the page renders unstyled. `TestGeneratedSiteLinksResolve`
  rejects relative links.
- **AA contrast for anything carrying text.** See above.
- **Focus must be visible.** Links have their underline removed and several
  controls drop their background and border, so the UA default ring is not
  enough. The `:focus-visible` ring is offset onto the page background so it
  stays legible over filled controls.
- **Nothing autoplays that a visitor cannot stop.** The demo recordings are
  `<video>` with controls, no `autoplay` attribute; script starts them only
  when `prefers-reduced-motion` is not set, and only while on screen. They
  were GIFs, which had no stop control at all.
- **`.reveal` starts at `opacity: 0`.** Anything that disables its transition
  has to restore opacity too, or the section stays permanently invisible. This
  is why the reduced-motion block and the `<noscript>` rule both exist.
- **One version string.** The verify page quotes `releaseVersion` from
  `site.json`; `TestReleaseVersionsAgree` checks it against
  `docs/verifying-releases.md` and `README.md`.
- **The keyboard reference is checked against the app.** Every verb in
  `internal/tui/verbs` must appear in `guide.html`, asserted by
  `TestKeyboardReferenceCoversVerbs`. A rebound or renamed key otherwise
  leaves the published reference plausible and wrong, which is worse than
  absent. Deliberate omissions go in that test's `omitted` map with a reason.
- **Product media is cache-busted at deploy.** GitHub Pages will not let us set
  `Cache-Control`, so `deploy-pages.yml` appends the commit SHA to every demo
  `.mp4`, `demo-*.png` poster and homepage `home-*.png` reference — across all
  pages, not a named one, which is how the step got missed when recordings
  moved before.
- **Every published capture is paired with the tape that produces it.**
  `TestRecordingTapesAndAssetsAgree` walks `docs/assets/tapes/` and fails on a
  tape with no committed output, an output with no tape, or a `Set Width`/
  `Set Height` that disagrees with the `<img>` dimensions on the homepage. The
  tape tree is the inventory; `index.html` and the tests read it rather than
  restating it.

## Provenance

Layout rhythm, pill geometry and the original two-font split were taken from
huly.io, including the retired aurora hero treatment. **The palette was never
taken from it** — it is kute's own semantic `Theme` tokens, reproduced in
HTML/CSS.
