package oidc

import (
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local: branding of the hosted pages.
//
// Every page carries the Claviger mark and a "Secured by Claviger" line. A client
// may add its own look: _oauthClients.logoUri (an https image) and accentColor
// (#rrggbb) show the calling app's logo and colour, so a person sees which app
// they are signing in to. Both are checked again when a page is rendered, and the
// strict Content-Security-Policy is only widened by the logo's own origin, on the
// page of that client. The accent colour is served as a small stylesheet from our
// own origin (style-src stays 'self'), never inlined.

//go:embed brand/*.svg
var brandFS embed.FS

var accentRegex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// brand is what a page needs of the calling client's look.
type brand struct {
	LogoURL   string // validated https image, empty for none
	AccentURL string // stylesheet that sets --accent, empty for none
}

func (m *module) brandOf(c *Client) brand {
	var b brand
	if c == nil || c.record == nil {
		return b
	}

	b.LogoURL = safeLogoURL(c.record.GetString("logoUri"))

	if accentRegex.MatchString(c.record.GetString("accentColor")) {
		b.AccentURL = mountPath + "/assets/accent.css?c=" + url.QueryEscape(c.ClientID)
	}

	return b
}

// safeLogoURL returns the URL when it is an https image address (or http on a
// loopback host, for development), and "" otherwise.
func safeLogoURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || len(raw) > 2000 {
		return ""
	}

	host := u.Hostname()
	if u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1")) {
		return u.String()
	}

	return ""
}

// withLogoOrigin widens img-src of a policy by the logo's origin.
func withLogoOrigin(csp, logo string) string {
	if logo == "" {
		return csp
	}

	u, err := url.Parse(logo)
	if err != nil {
		return csp
	}

	return strings.Replace(csp, "img-src 'self' data:", "img-src 'self' data: "+u.Scheme+"://"+u.Host, 1)
}

// accentCSS returns the stylesheet of a client's accent colour: the colour, and
// black or white text on it, whichever reads better.
func accentCSS(hex string) string {
	r, _ := strconv.ParseUint(hex[1:3], 16, 8)
	g, _ := strconv.ParseUint(hex[3:5], 16, 8)
	b, _ := strconv.ParseUint(hex[5:7], 16, 8)

	fg := "#ffffff"
	if (299*r+587*g+114*b)/1000 > 150 {
		fg = "#0c1220"
	}

	return fmt.Sprintf(":root { --accent: %s; --accent-fg: %s; }\n@media (prefers-color-scheme: dark) { :root { --accent: %s; --accent-fg: %s; } }\n", hex, fg, hex, fg)
}

func (m *module) serveAccent(e *core.RequestEvent) error {
	rec, err := m.app.FindFirstRecordByData(core.CollectionNameOAuthClients, "clientId", e.Request.URL.Query().Get("c"))
	if err != nil || rec.GetBool("disabled") || !accentRegex.MatchString(rec.GetString("accentColor")) {
		return e.NoContent(http.StatusNotFound)
	}

	h := e.Response.Header()
	h.Set("Content-Type", "text/css; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	_, err = e.Response.Write([]byte(accentCSS(rec.GetString("accentColor"))))

	return err
}

func serveBrandSVG(e *core.RequestEvent) error {
	name := e.Request.PathValue("name")
	if name != "claviger-mark.svg" && name != "claviger-mark-dark.svg" {
		return e.NoContent(http.StatusNotFound)
	}

	b, err := brandFS.ReadFile("brand/" + name)
	if err != nil {
		return e.NoContent(http.StatusNotFound)
	}

	h := e.Response.Header()
	h.Set("Content-Type", "image/svg+xml")
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	_, err = e.Response.Write(b)

	return err
}
