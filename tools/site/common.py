import html

PAGES = [
    ("index.html", "Overview"),
    ("architecture.html", "Architecture"),
    ("security.html", "Security"),
    ("benchmarks.html", "Benchmarks"),
    ("docs.html", "Docs"),
]
GH = "https://github.com/chxperiments/box"


# GitHub's mark, from @primer/octicons (mark-github-16).
GH_MARK = ("M6.766 11.328c-2.063-.25-3.516-1.734-3.516-3.656 0-.781.281-1.625.75-2.188-.203-.515-.172-1.609.063-2.062.625-.078 1.468.25 1.968.703.594-.187 1.219-.281 1.985-.281.765 0 1.39.094 1.953.265.484-.437 1.344-.765 1.969-.687.218.422.25 1.515.046 2.047.5.593.766 1.39.766 2.203 0 1.922-1.453 3.375-3.547 3.64.531.344.89 1.094.89 1.954v1.625c0 .468.391.734.86.547C13.781 14.359 16 11.53 16 8.03 16 3.61 12.406 0 7.984 0 3.563 0 0 3.61 0 8.031a7.88 7.88 0 0 0 5.172 7.422c.422.156.828-.125.828-.547v-1.25c-.219.094-.5.156-.75.156-1.031 0-1.64-.562-2.078-1.609-.172-.422-.36-.672-.719-.719-.187-.015-.25-.093-.25-.187 0-.188.313-.328.625-.328.453 0 .844.281 1.25.86.313.452.64.655 1.031.655s.641-.14 1-.5c.266-.265.47-.5.657-.656")


def logo():
    """The box wordmark, set in Geist SemiBold and stored as outlines
    (docs/assets/box-wordmark.svg), one path per letter so each can
    move on its own. Solid for the nav, hollow and drawn in for the footer."""
    import re
    from pathlib import Path
    svg = (Path(__file__).resolve().parents[2] / "docs/assets/box-wordmark.svg").read_text()
    vb = re.search(r'viewBox="([^"]+)"', svg).group(1)
    letters = re.findall(r' d="([^"]+)"', svg)
    x, y, w, h = (float(v) for v in vb.split())
    nav = "".join(f'<path style="--i:{i}" d="{d}"/>' for i, d in enumerate(letters))
    foot = "".join(f'<path pathLength="1" style="--i:{i}" d="{d}"/>' for i, d in enumerate(letters))
    pad = 30
    return (f'<svg class="logo" viewBox="{vb}" aria-hidden="true">{nav}</svg>',
            f'<svg viewBox="{x - pad:.0f} {y - pad:.0f} {w + 2 * pad:.0f} {h + 2 * pad:.0f}">{foot}</svg>')


def page(filename, title, description, body):
    LOGO, WORDMARK = logo()
    def links(*files):
        return "\n".join(
            f'    <a href="{f}"{" aria-current=\"page\"" if f == filename else ""}>{t}</a>'
            for f, t in PAGES if f in files
        )
    left, right = links("security.html", "benchmarks.html"), links("architecture.html", "docs.html")
    full_title = "box" if filename == "index.html" else f"{title} | box"
    return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{full_title}</title>
<meta name="description" content="{html.escape(description)}">
<meta property="og:title" content="{full_title}">
<meta property="og:description" content="{html.escape(description)}">
<meta name="theme-color" content="#000000">
<link rel="icon" href="assets/favicon.svg" type="image/svg+xml">
<link rel="icon" href="assets/favicon-32.png" sizes="32x32" type="image/png">
<link rel="apple-touch-icon" href="assets/apple-touch-icon.png">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Geist:wght@200..500&family=IBM+Plex+Mono:wght@400;500;600&family=IBM+Plex+Sans:wght@400;500;600&display=swap">
<link rel="stylesheet" href="assets/site.css">
</head>
<body>
<header class="nav invert">
  <nav class="nav-links nav-left" aria-label="Security and benchmarks">
{left}
  </nav>
  <div class="nav-mid">
    <a class="brand" href="index.html" aria-label="box, home">{LOGO}</a>
    <a class="nav-gh" href="{GH}" aria-label="box on GitHub"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="{GH_MARK}"/></svg></a>
  </div>
  <nav class="nav-links nav-right" aria-label="Architecture and docs">
{right}
  </nav>
</header>
<main id="top">
{body}
</main>
<footer class="footer invert wipe">
  <div class="wrap">
    <div class="footer-top">
      <div>
        <p class="footer-lead">A box for your AI.</p>
        <div class="install">
          <code><span class="p">$ </span>curl -fsSL https://chxperiments.github.io/box/install.sh | sh</code>
          <button class="copy" type="button" data-copy="curl -fsSL https://chxperiments.github.io/box/install.sh | sh">Copy</button>
        </div>
      </div>
      <nav aria-label="Project">
        <h4>Project</h4>
        <a href="architecture.html">Architecture</a>
        <a href="security.html">Security</a>
        <a href="benchmarks.html">Benchmarks</a>
        <a href="docs.html">Docs</a>
      </nav>
      <nav aria-label="Build with it">
        <h4>Build with it</h4>
        <a href="docs.html#sdk">Python SDK</a>
        <a href="docs.html#sdk">TypeScript SDK</a>
        <a href="docs.html#sdk">Go SDK</a>
        <a href="docs.html#sdk">Rust SDK</a>
        <a href="docs.html#mcp">MCP server</a>
        <a href="docs.html#api">Local API</a>
      </nav>
      <nav aria-label="Source">
        <h4>Source</h4>
        <a href="{GH}">GitHub</a>
        <a href="{GH}/blob/main/SECURITY.md">Threat model</a>
        <a href="{GH}/tree/main/bench">Benchmark code</a>
        <a href="{GH}/tree/main/examples">Examples</a>
      </nav>
    </div>
    <div class="wordmark" aria-hidden="true">{WORDMARK}</div>
    <div class="footer-base">
      <span>MIT licensed. Isolated, disposable microVM sandboxes.</span>
      <a href="#top">Back to top</a>
    </div>
  </div>
</footer>
<script src="assets/site.js" defer></script>
</body>
</html>
"""


def codebox(group, samples, label=None):
    """samples: list of (lang, code-html). One tab per language."""
    tabs = "".join(
        f'<button class="tab" role="tab" data-lang="{lang}" aria-selected="{"true" if i == 0 else "false"}" tabindex="{0 if i == 0 else -1}">{name}</button>'
        for i, (lang, name, _) in enumerate(samples)
    )
    # The first language shows without JavaScript; the script then applies
    # the reader's saved choice.
    panels = "".join(
        f'<pre class="code" data-lang-panel="{lang}"{"" if i == 0 else " hidden"}>{code}</pre>'
        for i, (lang, _, code) in enumerate(samples)
    )
    aria = f' aria-label="{label}"' if label else ""
    return f"""<div class="codebox" data-lang-group="{group}">
  <div class="codebox-bar" role="tablist"{aria}>{tabs}<button class="copy" type="button" data-copy-from>Copy</button></div>
  {panels}
</div>"""


def esc(s):
    return html.escape(s, quote=False)
