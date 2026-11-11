package apis

import (
	"encoding/json"
	"html/template"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/i18n"
)

// Fork-local (Claviger #97): the language of a hosted page.
//
// A page asks for its [PageText] once, renders its messages with T / HTML and
// puts DataBlock into the head: the messages its script needs (the "js."
// keys) travel as an inline JSON data block. A block of type
// application/json is data, not script, so the strict Content-Security-Policy
// (no inline script) stays as it is.

// PageText renders the messages of one page in the language negotiated for the request.
type PageText struct {
	Lang string // the language tag, for the lang attribute
	RTL  bool   // true for right-to-left languages
	b    *i18n.Bundle
}

// NewPageText negotiates the language of the request: ?lang=, the lang cookie,
// recordLang (the preferred language of the record the page is about; may be
// empty), Accept-Language, then meta.defaultLanguage. A valid ?lang= is
// remembered in the lang cookie.
func NewPageText(e *core.RequestEvent, recordLang string) *PageText {
	return NewPageTextDefault(e, recordLang, e.App.Settings().Meta.DefaultLang())
}

// NewPageTextDefault is NewPageText with another last-resort language: a page
// about an org passes core.OrgDefaultLanguage, so the org's own locale applies
// when the person has made no choice of their own.
func NewPageTextDefault(e *core.RequestEvent, recordLang, def string) *PageText {
	b := core.I18n(e.App)

	b.RememberChoice(e.Response, e.Request)

	lang := b.FromRequest(e.Request, recordLang, def)

	return &PageText{Lang: lang, RTL: i18n.Dir(lang) == "rtl", b: b}
}

// T returns a message as text; html/template escapes it where it is used.
func (p *PageText) T(key string, args ...i18n.Args) string {
	return p.b.T(p.Lang, key, args...)
}

// HTML returns a message that contains markup (the catalog is trusted). The
// arguments are escaped, except those of type template.HTML.
func (p *PageText) HTML(key string, args ...i18n.Args) template.HTML {
	escaped := make([]i18n.Args, len(args))

	for i, a := range args {
		escaped[i] = i18n.Args{}

		for k, v := range a {
			if h, ok := v.(template.HTML); ok {
				escaped[i][k] = string(h)
			} else {
				escaped[i][k] = template.HTMLEscaper(v)
			}
		}
	}

	return template.HTML(p.b.T(p.Lang, key, escaped...)) //nolint:gosec // the catalog is trusted, the arguments are escaped
}

// Args builds the arguments of a message from key, value pairs.
func (p *PageText) Args(kv ...any) i18n.Args {
	a := i18n.Args{}

	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			a[k] = kv[i+1]
		}
	}

	return a
}

// Has reports whether the catalog has the key in the language or a fallback.
func (p *PageText) Has(key string) bool {
	_, ok := p.b.Lookup(p.Lang, key)

	return ok
}

// RoleLabel is the name of an org role in the language, the role itself when
// the catalog has no name for it.
func (p *PageText) RoleLabel(role string) string {
	if key := "js.role." + role; p.Has(key) {
		return p.T(key)
	}

	return role
}

// Attrs is the extra attribute text of <html>: nothing for left-to-right
// languages, ` dir="rtl"` for right-to-left ones.
func (p *PageText) Attrs() template.HTMLAttr {
	if p.RTL {
		return ` dir="rtl"`
	}

	return ""
}

// DataBlock is the inline JSON data block with the "js." messages for the
// page script, followed by a newline.
func (p *PageText) DataBlock() template.HTML {
	raw, err := json.Marshal(p.b.Subset(p.Lang, "js."))
	if err != nil {
		raw = []byte("{}")
	}

	// json.Marshal escapes <, > and & so the block cannot end itself early
	return template.HTML(`<script type="application/json" id="i18n">` + string(raw) + "</script>\n") //nolint:gosec
}

// LangAttr renders ` lang="xx"` plus the direction, for hand-built documents.
func (p *PageText) LangAttr() string {
	s := ` lang="` + p.Lang + `"`
	if p.RTL {
		s += ` dir="rtl"`
	}

	return s
}
