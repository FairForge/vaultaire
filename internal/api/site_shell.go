package api

// Shared chrome for the server-rendered public sub-pages (/docs/*, /changelog)
// so they match the landing page: charcoal app bar with the pixel box mark,
// light-grey canvas, square white card, bold-then-light headings, yellow
// accents (SELFBOX-style, see landing.html). Deliberately no embedded fonts —
// the landing page carries Montserrat/Silkscreen; these pages fall back to the
// system sans and stay a few KB.

// siteShellStyle is the favicon link + <style> block (light + dark via
// prefers-color-scheme); the favicon is the landing page's pixel box mark.
const siteShellStyle = `<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%22-4 -4 22 22%22 shape-rendering=%22crispEdges%22%3E%3Crect x=%22-4%22 y=%22-4%22 width=%2222%22 height=%2222%22 fill=%22%23333%22/%3E%3Cpath fill=%22%23fff%22 d=%22M0 0h14v1h-14zM0 1h1v1h-1zM13 1h1v1h-1zM0 2h1v1h-1zM13 2h1v1h-1zM0 3h1v1h-1zM13 3h1v1h-1zM0 4h1v1h-1zM13 4h1v1h-1zM0 5h1v1h-1zM13 5h1v1h-1zM13 6h1v1h-1zM13 7h1v1h-1zM13 8h1v1h-1zM0 9h4v1h-4zM13 9h1v1h-1zM0 10h1v1h-1zM13 10h1v1h-1zM0 11h4v1h-4zM13 11h1v1h-1zM3 12h1v1h-1zM13 12h1v1h-1zM0 13h4v1h-4zM5 13h9v1h-9z%22/%3E%3C/svg%3E">
<style>
*, *::before, *::after { margin: 0; padding: 0; box-sizing: border-box; }
:root {
  --bg: #f2f2f2; --card: #ffffff; --panel: #e8e8e8; --ink: #2b2b2b; --body: #4d4d4d;
  --dim: #737373; --line: #dedede; --bar: #383838; --yellow: #ffd400; --hl: #fff4bf;
  --code-bg: #262626; --code-ink: #e6e6e6;
  --sans: 'Montserrat', 'Avenir Next', 'Segoe UI', system-ui, -apple-system, sans-serif;
  --mono: ui-monospace, 'SF Mono', SFMono-Regular, Menlo, Consolas, monospace;
  color-scheme: light dark;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #1a1a1a; --card: #242424; --panel: #2f2f2f; --ink: #f3f3f3; --body: #cfcfcf;
    --dim: #a0a0a0; --line: #363636; --bar: #101010; --hl: rgba(255, 212, 0, 0.12); --code-bg: #111111;
  }
}
body { font-family: var(--sans); background: var(--bg); color: var(--body); line-height: 1.65; -webkit-font-smoothing: antialiased; }
.top { position: sticky; top: 0; z-index: 10; background: var(--bar); }
.top-in { max-width: 860px; margin: 0 auto; padding: 0 20px; height: 56px; display: flex; align-items: center; gap: 18px; }
.brand { display: inline-flex; align-items: center; gap: 10px; color: #fff; text-decoration: none; font-weight: 800; font-size: 16px; letter-spacing: 0.02em; text-transform: uppercase; }
.brand svg { width: 28px; height: 28px; shape-rendering: crispEdges; flex: none; }
.crumb { color: rgba(255, 255, 255, 0.78); text-decoration: none; font-size: 14px; font-weight: 600; }
.crumb:hover { color: #fff; }
.crumb::before { content: "/"; margin-right: 12px; color: rgba(255, 255, 255, 0.35); }
main { max-width: 860px; margin: 32px auto 64px; padding: 40px 44px 48px; background: var(--card); }
@media (max-width: 900px) { main { margin: 0 0 40px; padding: 28px 16px 36px; } }
main h1 { font-size: clamp(1.9rem, 4vw, 2.5rem); font-weight: 800; letter-spacing: -0.02em; line-height: 1.15; color: var(--ink); margin-bottom: 0.75rem; }
main h2 { font-size: 1.3rem; font-weight: 800; letter-spacing: -0.01em; color: var(--ink); margin: 2.25rem 0 0.75rem; padding-top: 1.25rem; border-top: 1px solid var(--line); }
main h3 { font-size: 1.05rem; font-weight: 700; color: var(--ink); margin: 1.5rem 0 0.5rem; }
main p { margin: 0.75rem 0; }
main ul, main ol { margin: 0.75rem 0; }
main li { margin: 0.45rem 0 0.45rem 1.25rem; }
main ul { list-style: none; }
main ul > li { position: relative; margin-left: 0; padding-left: 20px; }
main ul > li::before { content: ""; position: absolute; left: 0; top: 0.6em; width: 8px; height: 8px; background: #27c24c; }
main strong { color: var(--ink); }
main a { color: var(--ink); text-decoration: underline; text-decoration-thickness: 2px; text-underline-offset: 3px; text-decoration-color: var(--yellow); }
main a:hover { background: var(--hl); }
main code { font: 0.88em var(--mono); background: var(--panel); color: var(--ink); padding: 1px 6px; }
main pre { background: var(--code-bg); color: var(--code-ink); padding: 16px 18px; overflow-x: auto; margin: 0.9rem 0; border-left: 6px solid var(--yellow); }
main pre code { background: none; color: inherit; padding: 0; font-size: 13px; line-height: 1.6; }
main blockquote { margin: 1rem 0; padding: 12px 16px; background: var(--panel); border-left: 6px solid var(--ink); }
main table { border-collapse: collapse; margin: 0.9rem 0; width: 100%; display: block; overflow-x: auto; }
main th, main td { padding: 0.55rem 0.8rem; text-align: left; border-bottom: 1px solid var(--line); font-size: 0.94em; }
main th { color: var(--ink); font-weight: 700; }
main hr { border: none; border-top: 1px solid var(--line); margin: 1.5rem 0; }
:focus-visible { outline: 3px solid var(--yellow); outline-offset: 2px; }
::selection { background: var(--yellow); color: #111; }
</style>
`

// siteShellBrand is the bar's home link: the 14×14 pixel box mark (same
// geometry as the landing page's #s-mark sprite) plus the wordmark.
const siteShellBrand = `<a class="brand" href="/" aria-label="stored.ge home">` +
	`<svg viewBox="0 0 14 14" aria-hidden="true" focusable="false"><path fill="currentColor" d="` +
	`M0 0h14v1h-14zM0 1h1v5h-1zM13 1h1v13h-1zM0 9h4v1h-4zM0 10h1v1h-1zM0 11h4v1h-4zM3 12h1v1h-1zM0 13h4v1h-4zM5 13h8v1h-8z` +
	`"/></svg><span>stored.ge</span></a>`
