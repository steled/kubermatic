/*
Copyright 2026 The Kubermatic Kubernetes Platform contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maintenance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"
)

const (
	defaultTitle   = "Scheduled maintenance"
	defaultMessage = "The platform is currently being updated. Please try again in a few minutes."
)

// Branding is the subset of the dashboard branding configuration
// (spec.ui.config -> "branding") that is used to render the maintenance page,
// so the page matches the look of the dashboard.
type Branding struct {
	ProductName      string         `json:"product_name,omitempty"`
	Tagline          string         `json:"tagline,omitempty"`
	PostfixPageTitle string         `json:"postfix_page_title,omitempty"`
	LogoURL          string         `json:"logo_url,omitempty"`
	LogoDarkURL      string         `json:"logo_dark_url,omitempty"`
	FaviconURL       string         `json:"favicon_url,omitempty"`
	FontFamily       string         `json:"font_family,omitempty"`
	Colors           BrandingColors `json:"colors,omitempty"`
}

type BrandingColors struct {
	Primary    string `json:"primary,omitempty"`
	HeaderBG   string `json:"header_bg,omitempty"`
	HeaderText string `json:"header_text,omitempty"`
}

// ParseBranding extracts the branding block from the dashboard configuration
// (the JSON string in KubermaticConfiguration spec.ui.config). An empty or
// branding-less configuration yields an empty Branding.
func ParseBranding(uiConfig string) (Branding, error) {
	if strings.TrimSpace(uiConfig) == "" {
		return Branding{}, nil
	}

	var cfg struct {
		Branding Branding `json:"branding"`
	}
	if err := json.Unmarshal([]byte(uiConfig), &cfg); err != nil {
		return Branding{}, fmt.Errorf("failed to parse spec.ui.config: %w", err)
	}

	return cfg.Branding, nil
}

// PageOptions controls the content of the maintenance page.
type PageOptions struct {
	// Title is the headline. Defaults to "Scheduled maintenance".
	Title string
	// Message is the explanatory text. Defaults to a generic message.
	Message string
	// Until is a free-form text for the expected end, e.g. "2026-11-10 08:00 CET".
	Until    string
	Branding Branding
	// InvertLogoInDark inverts the logo in dark mode. It is used for the default
	// logo of the dashboard, which is dark blue and would vanish on a dark background.
	InvertLogoInDark bool
}

var (
	hexColorRegex   = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)
	fontFamilyRegex = regexp.MustCompile(`^[a-zA-Z0-9 ,'"_-]{1,200}$`)
	dataImageRegex  = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp|svg\+xml|x-icon|vnd\.microsoft\.icon)[;,]`)
)

// safeURL returns the URL only if it can be loaded by a browser from the
// maintenance page. Relative paths (the dashboard allows files mounted into
// its container) are dropped, because they would not resolve while the
// dashboard is replaced by this page.
func safeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if dataImageRegex.MatchString(raw) {
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}

	return u.String()
}

func safeColor(c, fallback string) string {
	if hexColorRegex.MatchString(c) {
		return c
	}

	return fallback
}

type pageData struct {
	Title      string
	Heading    string
	Message    string
	Until      string
	Tagline    string
	LogoURL    template.URL
	LogoDark   template.URL
	Favicon    template.URL
	ProductAlt string
	FontFamily template.CSS
	Primary    template.CSS
	HeaderBG   template.CSS
	HeaderText template.CSS
	InvertDark bool
}

var pageTemplate = template.Must(template.New("maintenance").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{ .Title }}</title>
{{- if .Favicon }}
<link rel="icon" href="{{ .Favicon }}">
{{- end }}
<style>
:root { --primary: {{ .Primary }}; --header-bg: {{ .HeaderBG }}; --header-text: {{ .HeaderText }}; --surface: #fff; --surface-text: #4d555c; --default-primary: #00517d; --bg: #f5f6f8; --card: #fff; --text: #1d2330; --muted: #5d6677; }
@media (prefers-color-scheme: dark) { :root { --surface: #2b2b2c; --surface-text: #eceef3; --default-primary: #0072b0; --bg: #14171f; --card: #1e222c; --text: #eceef3; --muted: #a2a9b8; } header.has-dark .light { display: none; } header.has-dark .dark { display: inline; } header.invert .logo { filter: brightness(0) invert(1) opacity(.85); } }
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: flex; flex-direction: column; background: var(--bg); color: var(--text); font-family: {{ .FontFamily }}; }
header { background: var(--header-bg); color: var(--header-text); padding: 16px 24px; min-height: 64px; display: flex; align-items: center; }
header img { max-height: 32px; max-width: 220px; }
header .dark { display: none; }
main { flex: 1; display: flex; align-items: center; justify-content: center; padding: 24px 16px; }
.card { background: var(--card); border-radius: 8px; box-shadow: 0 2px 12px rgba(0,0,0,.12); padding: 40px; max-width: 560px; width: 100%; text-align: center; border-top: 4px solid var(--primary); }
h1 { margin: 0 0 16px; font-size: 1.6rem; }
p { margin: 0 0 12px; line-height: 1.5; color: var(--muted); }
.until { color: var(--text); font-weight: 600; }
</style>
</head>
<body>
<header class="{{ if .LogoDark }}has-dark{{ end }}{{ if .InvertDark }} invert{{ end }}">
{{- if .LogoURL }}
<img class="logo light" src="{{ .LogoURL }}" alt="{{ .ProductAlt }}">
{{- if .LogoDark }}
<img class="logo dark" src="{{ .LogoDark }}" alt="{{ .ProductAlt }}">
{{- end }}
{{- else if .ProductAlt }}
<strong>{{ .ProductAlt }}</strong>
{{- end }}
</header>
<main>
<div class="card" role="status">
<h1>{{ .Heading }}</h1>
<p>{{ .Message }}</p>
{{- if .Until }}
<p class="until">Expected until: {{ .Until }}</p>
{{- end }}
{{- if .Tagline }}
<p>{{ .Tagline }}</p>
{{- end }}
</div>
</main>
</body>
</html>
`))

// RenderPage renders the HTML maintenance page.
func RenderPage(opts PageOptions) ([]byte, error) {
	b := opts.Branding

	heading := opts.Title
	if heading == "" {
		heading = defaultTitle
	}

	message := opts.Message
	if message == "" {
		message = defaultMessage
	}

	title := heading
	if b.PostfixPageTitle != "" {
		title = heading + " " + b.PostfixPageTitle
	}

	logo := safeURL(b.LogoURL)
	logoDark := safeURL(b.LogoDarkURL)
	if logo == "" {
		// a dark logo without a light logo would only be shown half of the time
		logoDark = ""
	}

	fontFamily := "system-ui, -apple-system, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif"
	if fontFamilyRegex.MatchString(b.FontFamily) {
		fontFamily = b.FontFamily
	}

	data := pageData{
		Title:      title,
		Heading:    heading,
		Message:    message,
		Until:      opts.Until,
		Tagline:    b.Tagline,
		LogoURL:    template.URL(logo),                  // #nosec G203 -- validated by safeURL
		LogoDark:   template.URL(logoDark),              // #nosec G203 -- validated by safeURL
		Favicon:    template.URL(safeURL(b.FaviconURL)), // #nosec G203 -- validated by safeURL
		ProductAlt: b.ProductName,
		InvertDark: opts.InvertLogoInDark && logo != "",
		FontFamily: template.CSS(fontFamily),                                            // #nosec G203 -- validated by fontFamilyRegex
		Primary:    template.CSS(safeColor(b.Colors.Primary, "var(--default-primary)")), // #nosec G203 -- validated by hexColorRegex
		HeaderBG:   template.CSS(safeColor(b.Colors.HeaderBG, "var(--surface)")),        // #nosec G203 -- validated by hexColorRegex
		HeaderText: template.CSS(safeColor(b.Colors.HeaderText, "var(--surface-text)")), // #nosec G203 -- validated by hexColorRegex
	}

	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to render maintenance page: %w", err)
	}

	return buf.Bytes(), nil
}

// RenderJSON renders the response body for non-browser (API) clients.
func RenderJSON(opts PageOptions) ([]byte, error) {
	message := opts.Message
	if message == "" {
		message = defaultMessage
	}

	body := map[string]any{
		"error": map[string]any{
			"code":    503,
			"message": message,
		},
	}
	if opts.Until != "" {
		body["until"] = opts.Until
	}

	return json.Marshal(body)
}
