---
name: kute.dev
description: The incident console for Kubernetes — dark, terminal-native marketing site whose palette mirrors the real product
colors:
  bg: "#0b0b10"
  bg-chrome: "#0e0e15"
  bg-strip: "#0c0c12"
  bg-card: "#111119"
  bg-card-hi: "#16121e"
  bg-palette: "#101018"
  border: "#26263a"
  border-subtle: "#1c1c2c"
  text: "#f0f0fa"
  text-primary: "#d8d8e8"
  text-secondary: "#9a9ab2"
  text-dim: "#8f8fa4"
  text-faint: "#83839f"
  text-ghost: "#78789b"
  accent: "#a78bfa"
  accent-hi: "#c4b5fd"
  sel-bg: "#1d1633"
  good: "#34d17b"
  warn: "#e8c74a"
  bad: "#ef6a6a"
  bad-soft: "#ef8a8a"
  bad-text: "#f0b7b7"
  info: "#6aa8ef"
  home-accent-dark: "#5ddba4"
  home-accent-light: "#12855a"
  home-bg-dark: "#101014"
  home-bg-light: "#f7f7fa"
typography:
  display:
    fontFamily: "Sora, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(38px, 6vw, 68px)"
    fontWeight: 600
    lineHeight: 1.02
    letterSpacing: "-0.03em"
  headline:
    fontFamily: "Sora, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(28px, 3.4vw, 40px)"
    fontWeight: 600
    lineHeight: 1.1
    letterSpacing: "-0.03em"
  title:
    fontFamily: "Sora, ui-sans-serif, system-ui, sans-serif"
    fontSize: "21px"
    fontWeight: 600
    lineHeight: 1.3
    letterSpacing: "-0.01em"
  body:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, sans-serif"
    fontSize: "16px"
    fontWeight: 400
    lineHeight: 1.5
    letterSpacing: "-0.01em"
  label:
    fontFamily: "'JetBrains Mono', ui-monospace, 'SF Mono', Menlo, monospace"
    fontSize: "12px"
    fontWeight: 500
    lineHeight: 1.3
    letterSpacing: "0.08em"
  mono:
    fontFamily: "'JetBrains Mono', ui-monospace, 'SF Mono', Menlo, monospace"
    fontSize: "12.5px"
    fontWeight: 400
    lineHeight: 1.5
    letterSpacing: "normal"
  home-display:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(44px, 6vw, 68px)"
    fontWeight: 500
    lineHeight: 1.02
    letterSpacing: "-0.035em"
  home-emphasis:
    fontFamily: "Newsreader, Georgia, serif"
    fontStyle: italic
    fontWeight: 400
  home-mono:
    fontFamily: "'Geist Mono', ui-monospace, Menlo, monospace"
rounded:
  card: "14px"
  pill: "9999px"
  term: "10px"
  home-control: "8px"
  home-frame: "12px"
  home-panel: "16px"
spacing:
  section-gap: "96px"
  gutter: "24px"
components:
  button-primary:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.bg}"
    rounded: "{rounded.pill}"
    padding: "12px 24px"
  button-primary-hover:
    backgroundColor: "{colors.accent-hi}"
  button-ghost:
    backgroundColor: "transparent"
    textColor: "{colors.text}"
    rounded: "{rounded.pill}"
    padding: "12px 24px"
  button-ghost-hover:
    textColor: "{colors.accent-hi}"
  card:
    backgroundColor: "{colors.bg-card}"
    rounded: "{rounded.card}"
    padding: "32px"
  eyebrow:
    textColor: "{colors.accent-hi}"
    rounded: "{rounded.pill}"
    padding: "5px 14px"
    typography: "{typography.label}"
  kbd:
    backgroundColor: "{colors.sel-bg}"
    textColor: "{colors.accent-hi}"
    padding: "2px 7px"
    typography: "{typography.mono}"
---

# Design System: kute.dev

## Overview

**Creative North Star: "The Incident Console"**

kute.dev is dark by default, terse in its type, and precise about what it's showing — because it's the marketing site for a tool whose entire premise is showing an engineer the truth about a broken cluster without decoration. The palette isn't a marketing choice layered on top of the product; it *is* the product's own semantic `Theme` (Good/Warn/Bad/Info, the same accent violet), reproduced in HTML/CSS so the terminal mockups on the page look like the real screens a visitor will see the moment they install. Layout rhythm, pill-shaped buttons, and the original two-font split were borrowed from huly.io's system — a deliberate starting point, documented in `docs/design/website.md`'s Provenance section — while the universal editorial shell now uses Geist, Geist Mono, Newsreader, and green interaction signals.

The system reads as engineered calm: generous whitespace and a slow section rhythm (96px between sections) carry the pitch, while terminal views embedded in that whitespace are dense, monospaced, and exact — status dots, keybar hints, condition messages shown verbatim, the same visual grammar as the app. The homepage's task explorer uses captures from the real demo binary; hand-built mockups remain supporting illustrations elsewhere. Nothing on the page claims more than the product does; the design's only job is to get out of the way of that claim.

**Editorial shell.** Every page shares an editorial, answer-first shell scoped by `body.editorial-page`: Geist and Geist Mono replace the original shared typefaces, Newsreader italic gives display copy a human counterpoint, and green becomes the interaction signal in both themes. The compact navigation, left-aligned page headers, paper-like light surfaces, near-black dark surfaces, and compact footer are universal. Homepage-only structures such as the task explorer remain scoped by `body.home-page`. Guide's embedded `.term` mockups re-establish the TUI's violet palette and JetBrains Mono at their boundary, while real demo screenshots always use their own native theme colors and switch with the website theme.

**Key Characteristics:**
- Dark-first, both themes fully supported (light is not an afterthought — see the Colors section's AA note)
- A single true accent (Signal Violet) used sparingly against a mostly monochrome dark canvas
- Flat surfaces separated by tonal steps and thin borders, not shadow stacks
- A task-oriented product explorer pairing kute-like selected rows with real demo captures — the site's proof is shown, not described
- Sora for headlines, Inter for reading prose, JetBrains Mono for anything that is or resembles terminal output

## Colors

A near-monochrome dark canvas (five stepped tonal surfaces) with one true accent and the product's own four-color status vocabulary — the same roles a visitor will see inside the actual TUI.

### Primary
- **Signal Violet** (`#a78bfa` dark / `#6b46d9` light — `colors.accent`): the product-proof accent. Terminal mockup links, selection state, and active indicators use it inside product boundaries; the editorial shell uses green for interaction.
- **Signal Violet, Bright** (`#c4b5fd` dark / `#5936b8` light — `colors.accent-hi`): hover/active state for accent elements, and the color used for emphasized inline terms (`em` in headlines, crumb kind names in `.term` mockups).

### Neutral (surfaces, dark → light stepped)
- **Void** (`#0b0b10` — `colors.bg`): page background.
- **Chrome** (`#0e0e15` — `colors.bg-chrome`): nav bar, terminal mockup title/header bars — the "frame" tone.
- **Strip** (`#0c0c12` — `colors.bg-strip`): the thin status-strip band inside terminal mockups.
- **Card** (`#111119` — `colors.bg-card`): pain cards and install panels — the "content sits here" surface.
- **Card, Raised** (`#16121e` — `colors.bg-card-hi`): a half-step lighter, tinted slightly toward the accent hue; used where a card needs to read as marginally elevated without a shadow (e.g. the goto-palette mockup body).
- **Selection Wash** (`#1d1633` — `colors.sel-bg`): the accent-tinted background for a selected/hovered row inside a terminal mockup, and real text selection (`::selection`) on the page itself.

### Neutral (text, dark → light stepped)
- **Text, Loud** (`#f0f0fa` — `colors.text`): headings only.
- **Text, Primary** (`#d8d8e8` — `colors.text-primary`): default body color, terminal mockup row text.
- **Text, Secondary** (`#9a9ab2` — `colors.text-secondary`): supporting prose — section subheads, card copy.
- **Text, Dim / Faint / Ghost** (`#8f8fa4` / `#83839f` / `#78789b` — `colors.text-dim` / `text-faint` / `text-ghost`): progressively quieter captions, metadata, footnotes, and terminal-mockup chrome labels.

### Status (borrowed directly from the TUI's semantic Theme)
- **Terminal Green** (`#34d17b` — `colors.good`): healthy/success state, both in terminal mockups and on the page itself (e.g. deep-dive checkmarks).
- **Warm Amber** (`#e8c74a` — `colors.warn`): caution/degraded state.
- **Alert Red** (`#ef6a6a` — `colors.bad`, with `bad-soft` / `bad-text` variants): destructive/error state — reserved for genuinely alarming content (the type-the-name modal mockup, error banners), never decoration.
- **Steady Blue** (`#6aa8ef` — `colors.info`): informational/neutral-notable state (all-namespaces badges, live-connection indicators).

### Named Rules
**The Theme-Mirror Rule.** Every color on this site except the quiet text tiers is the TUI's own semantic `Theme` token, not an independent marketing palette. A visitor who installs kute should see the exact hues they just read about.

**The AA Quiet-Tier Rule.** `text-dim`, `text-faint` and `text-ghost` are deliberately *stronger* here than the equivalent TUI tokens. They carry real prose on a web page and must clear 4.5:1 contrast against `bg` — a terminal running under the user's own color scheme isn't held to that standard, but this site is. `TestTextContrastMeetsAA` enforces it in CI; do not re-sync these three tokens to the TUI's exact values.

**The One True Accent Rule.** Signal Violet is the only color used to mean "act here." It never doubles as a neutral decoration — if something is violet, it is interactive or it is quoting the product's own selection state.

## Typography

**Display Font:** Sora (with `ui-sans-serif, system-ui, sans-serif` fallback)
**Body Font:** Inter (with `ui-sans-serif, system-ui, -apple-system, sans-serif` fallback)
**Label/Mono Font:** JetBrains Mono (with `ui-monospace, 'SF Mono', Menlo, monospace` fallback)

**Character:** Sora is geometric and slightly condensed at display sizes — confident without shouting. Inter carries the actual argument in longer-form prose at a comfortable 1.5–1.7 line-height. JetBrains Mono is reserved for anything that is, quotes, or resembles terminal output — it's the tell that a piece of text is proof, not pitch.

### Hierarchy
- **Display** (600, `clamp(38px, 6vw, 68px)`, line-height 1.02): the hero `<h1>` only.
- **Headline** (600, `clamp(28px, 3.4vw, 40px)`, line-height 1.1): section `<h2>`s.
- **Title** (600, 21px, line-height 1.3): `<h3>`s — deep-dive and pain-card subheads.
- **Body** (400, 16px base; 17–18px for hero sub-copy and section-head intros, line-height 1.5–1.7): all reading prose. Guide-page paragraphs cap at 72ch.
- **Label** (500, 12px, letter-spacing 0.08em, uppercase): the `.eyebrow` badge and section-caption micro-labels (`SINGLE BINARY`, `CAPACITY`, etc.).
- **Mono** (400, 12–14px): terminal mockup body text, code blocks, `kbd` key badges, inline `<code>`.

### Named Rules
**The Mono-Means-Real Rule.** JetBrains Mono is never used decoratively. If a string is set in mono, it is a literal — a command, a key, a file path, or terminal-mockup content a visitor could actually see on their own screen.

## Layout

A centered `1180px` max-width column (`--max-width`) with `24px` side gutters below the breakpoint, and a slow `96px` rhythm between major sections (`--section-gap`). Hero and page-header bands get extra top padding to clear the fixed nav; every supporting-page header begins at `148px` on desktop and `124px` on mobile. The homepage explorer divides that column into a compact `260px` task rail and a stable media stage; below `720px` the rail becomes a horizontally scrolling tab list above the stage.

Two distinct page shapes exist: the marketing pages (`index`, most sections centered, two-up and three-up card grids) and the documentation page (`guide.html`), which drops the marketing grid for a `220px` sticky contents rail beside a `minmax(0, 1fr)` prose column — Read mode inside an otherwise Persuade site.

The Install guide uses the browser-reported platform as progressive emphasis: macOS/Linux or Windows receives a small “Your platform” label, and Windows moves ahead of macOS/Linux for Windows visitors. Both sections, manual downloads, and every anchor remain visible; detection never redirects or hides guide content.

Responsive behavior collapses at three breakpoints: `960px` folds the guide rail from a sidebar into a horizontal scrollable strip above the prose; `720px` turns the nav into a slide-down mobile menu and drops two/three-up grids to one column; `640px` lets terminal-mockup tables scroll horizontally rather than compressing columns into illegibility (`.term-body { overflow-x: auto }`) — the mockups' internal grid never reflows, it scrolls.

## Elevation & Depth

Mostly flat, tonal layering. Depth comes from five stepped background tones (`bg` → `bg-chrome` → `bg-strip` → `bg-card` → `bg-card-hi`) plus thin `1px` borders, not from a shadow stack — most cards (pain cards, deep-dive copy, the surface grid) carry no shadow at all. Shadows are reserved for a small set of surfaces that are meant to read as genuinely floating above the page: terminal mockups, the shared install panel, and the palette mockup, all using `shadow-xl` (`0 12px 40px rgba(0,0,0,.5)`). The violet system's glow effect (`shadow-glow`) is scoped to primary actions; the editorial shell deliberately removes it in favor of quieter contrast.

### Shadow Vocabulary
- **sm** (`0 4px 6px rgba(0,0,0,.25)`): reserved, lightly used.
- **md** (`0 4px 16px rgba(0,0,0,.35)`): reserved, lightly used.
- **xl** (`0 12px 40px rgba(0,0,0,.5)`): terminal mockups, install panel, palette mockup — anything meant to feel like a floating window.
- **glow** (`0 0 0 1px rgba(167,139,250,.25), 0 8px 30px rgba(167,139,250,.12)` dark / `rgba(107,70,217,...)` light): the primary CTA button and the install panel's ambient radial wash. The only shadow that carries the accent color instead of pure black.

### Named Rules
**The Flat-by-Default Rule.** A surface earns a shadow only by being a "floating window" (a terminal mockup, a panel meant to feel raised off the page) — not by being a card. Most content sits flat on its tonal step.

## Shapes

Two radius families cover almost everything: a **pill** (`9999px`) for every button, badge, and eyebrow — the site's signature "this is interactive" silhouette — and a **soft rectangle** (`14px` for cards/panels, `10px` for terminal-mockup windows) for anything that's a container. A handful of smaller controls (the `kbd` badge, `copy-btn`, inline `code`) use small untokenized radii in the 3–7px range for tighter, denser elements; the install panel's outer shell goes up to `20px` as its largest, calmest corner. Borders are hairline (`1px`, `border` or the quieter `border-subtle`) and do most of the separation work that shadows would otherwise do.

### Named Rules
**The Pill-Or-Container Rule.** If something is clickable on its own (button, badge, nav-star chip), it's a pill. If something holds other content (card, panel, terminal window), it's a soft rectangle. Nothing in between.

## Components

### Buttons
- **Shape:** full pill (`border-radius: 9999px`).
- **Primary:** solid Signal Violet background, text in `colors.bg` — the same color-on-accent pattern `.skip-link` already uses. `bg` flips to the correct contrast partner per theme (near-black in dark mode, near-white in light mode), which is what makes this the *right* token here rather than a fixed literal: a hardcoded near-black read fine in dark mode but only cleared ~3.3:1 against light mode's accent, under this system's own 4.5:1 bar. `var(--bg)` clears ~5.6:1 light / ~7.2:1 dark. Wrapped in the accent glow shadow. Hover swaps to `accent-hi` and intensifies the glow.
- **Ghost:** transparent fill, `border` token outline, `text` colored label. Hover shifts both border and text to `accent-hi` — no fill change, so it reads as secondary at every state.
- **Small variant (`btn-sm`):** tighter padding (`8px 18px`) and 13px label, used in the nav and wherever a button sits beside denser content.
- **Press feedback:** every button nudges `1px` down on `:active` — the only motion a button gets besides its hover transition.

### Chips / Badges
- **Eyebrow:** the mono, uppercase, pill-shaped label that opens the hero and most sections (`SINGLE BINARY`, etc.) — accent-hi text on a 10%-opacity accent wash with a 25%-opacity accent border. It's a quotation of the accent color at low intensity, not a full-strength badge.
- **Friction tier tags:** three-way status pill (`rev`/`del`/`prod`) coloring destructive-action tiers Terminal Green / Warm Amber / Alert Red at ~10–12% background opacity — the same three-tier vocabulary the product's own confirm flow uses.

### Cards / Containers
- **Corner Style:** `14px` (`rounded.card`).
- **Background:** `bg-card`, occasionally `bg-card-hi` for a half-step-raised feel.
- **Shadow Strategy:** none by default (see Elevation & Depth) — separation comes from the tonal step against the page background plus a `border-subtle` hairline.
- **Internal Padding:** `32px` for pain/deep-dive cards, up to `56px` for the full install panel.

### Navigation
- **Style:** fixed, transparent until scrolled, then gains a blurred (`backdrop-filter: blur(14px) saturate(140%)`) translucent chrome background and a bottom hairline border — a 250ms transition on background/border/padding, not a hard cut.
- **Typography:** brand wordmark in Sora 600 with the `❯` mark in Signal Violet; nav links in 14px Inter, `text-secondary` at rest, `text` on hover.
- **Mobile treatment:** below `720px` the nav links collapse into a slide-down panel (`chrome` background, full-bleed) toggled by a hamburger icon; the GitHub-star chip is dropped entirely to save width.

### The Product Explorer (homepage signature)
- **Structure:** six workflows in a vertical task rail, with one stable media stage. Incident diagnosis nests Cluster triage/Pod diagnosis/Certificates/Timeline; Navigation nests Goto/Namespace/Context; Batch debugging nests CronJob failure/Job attempts; Routing nests Ingress/HTTPRoute; GitOps nests Flux/Argo CD; Safety nests Non-production/Production confirmations.
- **Visual language:** the selected task quotes the TUI's accent rail and selection wash; the surrounding shell stays quiet so the real terminal capture remains dominant.
- **Media contract:** every screen opens on paired dark/light deterministic PNGs from `kute --demo`, selected with the website theme. Incident and routing items may replace that still with an explicitly started recording; changing tabs pauses and resets it. Nothing in the explorer autoplays.
- **Responsive contract:** below `720px` tasks become horizontal tabs. Below `640px` the capture pans inside its own full-width viewport rather than shrinking terminal text into illegibility.
- **Keyboard contract:** arrow keys follow tab orientation; Home and End jump to the edges. Every nested screen list uses Left/Right independently of the outer workflow tabs.

### The Terminal Mockup (supporting component)
The `.term` family is the site's proof, not its decoration — a hand-rebuilt HTML/CSS reproduction of kute's actual screens (title bar with traffic-light dots, breadcrumb header, status strip, table rows with health-colored glyphs, keybar) using the exact same semantic tokens as the real TUI. Variants exist for the destructive-action confirm modal (`.term-modal`, red-bordered to match the product's own reserved-red-for-destructive-confirms rule), the jump palette (`.term-palette`), the all-namespaces triage view with collapsible healthy groups (`.term-group-*`), log lines with severity coloring (`.term-log-line`), and masked/revealed Secret values (`.term-yaml-line`). Every mockup must stay visually faithful to what installing kute actually shows — this component is a promise, and its accuracy is a credibility question, not a cosmetic one.

## Do's and Don'ts

### Do:
- **Do** keep every color token identical in dark and light contexts to its counterpart in `internal/tui`'s semantic `Theme`, except the three quiet-tier text tokens, which are intentionally stronger here to clear WCAG AA on real prose.
- **Do** build and verify anything new in both themes, including under bare `prefers-color-scheme` with the manual toggle untouched — a selector list ending in a comma before an `@media` block silently drops the whole rule (this shipped a dark nav bar on light mode once).
- **Do** give any element that removes its default focus affordance (underline-less links, borderless buttons) a visible `:focus-visible` ring — the offset 2px accent ring, positioned to stay legible over filled controls too.
- **Do** keep `.term` mockups pixel-honest to the real product's Theme tokens, glyphs, and layout. If the TUI's design changes, the mockup that illustrates it changes with it.
- **Do** reserve the accent glow shadow (`shadow-glow`) for the single primary action on a given panel — it is the "act here" signal, and it stops working if more than one thing on screen has it.

### Don't:
- **Don't** apply a drop shadow to an ordinary card. Shadows are for surfaces that should read as floating windows (terminal mockups and install panels) — everything else separates by tonal step and hairline border alone.
- **Don't** let motion run without an escape hatch: any `.reveal`-style entrance animation must have its start state (`opacity: 0`) undone under `prefers-reduced-motion`, or the section stays permanently invisible for anyone with that preference set.
- **Don't** autoplay media a visitor can't stop. Demo recordings use native `<video controls>`, no `autoplay` attribute, started by script only when motion isn't reduced and only while the element is on screen.
- **Don't** use Alert Red for anything that isn't genuinely destructive or erroring. It's load-bearing exactly because it's rare — reserved for the type-the-name modal mockup, real error banners, and the `bad`-tier friction tag.
