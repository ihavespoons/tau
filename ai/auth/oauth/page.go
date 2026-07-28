package oauth

import "html"

const logoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 800 800" aria-hidden="true"><path fill="#fff" fill-rule="evenodd" d="M165.29 165.29 H517.36 V400 H400 V517.36 H282.65 V634.72 H165.29 Z M282.65 282.65 V400 H400 V282.65 Z"/><path fill="#fff" d="M517.36 400 H634.72 V634.72 H517.36 Z"/></svg>`

func renderPage(title, heading, message, details string) string {
	page := `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>` + html.EscapeString(title) + `</title>
  <style>
    :root { --text: #fafafa; --text-dim: #a1a1aa; --page-bg: #09090b; }
    * { box-sizing: border-box; }
    html { color-scheme: dark; }
    body {
      margin: 0; min-height: 100vh; display: flex; align-items: center;
      justify-content: center; padding: 24px; background: var(--page-bg);
      color: var(--text); text-align: center;
      font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    }
    main { width: 100%; max-width: 560px; display: flex; flex-direction: column; align-items: center; }
    .logo { width: 72px; height: 72px; display: block; margin-bottom: 24px; }
    h1 { font-size: 20px; font-weight: 600; margin: 0 0 12px; }
    p { margin: 0; color: var(--text-dim); line-height: 1.5; }
    pre {
      margin: 16px 0 0; padding: 12px; width: 100%; overflow-x: auto;
      text-align: left; color: var(--text-dim); font-size: 12px;
      font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    }
  </style>
</head>
<body>
  <main>
    <div class="logo">` + logoSVG + `</div>
    <h1>` + html.EscapeString(heading) + `</h1>
    <p>` + html.EscapeString(message) + `</p>`
	if details != "" {
		page += "\n    <pre>" + html.EscapeString(details) + "</pre>"
	}
	page += `
  </main>
</body>
</html>`
	return page
}

// SuccessHTML is the page shown after a successful OAuth callback.
func SuccessHTML(message string) string {
	return renderPage("Authentication complete", "Authentication complete", message, "")
}

// ErrorHTML is the page shown when an OAuth callback fails.
func ErrorHTML(message, details string) string {
	return renderPage("Authentication failed", "Authentication failed", message, details)
}
