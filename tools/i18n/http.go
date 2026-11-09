package i18n

import (
	"net/http"
	"time"
)

// CookieName is the cookie that remembers an explicit language choice.
const CookieName = "lang"

// QueryParam is the query parameter that chooses a language explicitly.
const QueryParam = "lang"

// FromRequest negotiates the language of a request: ?lang=, the lang cookie,
// recordLang (the preferred language of the record the page is about, may be
// empty), Accept-Language, then def.
func (b *Bundle) FromRequest(r *http.Request, recordLang, def string) string {
	p := Preference{
		Explicit:       r.URL.Query().Get(QueryParam),
		Record:         recordLang,
		AcceptLanguage: r.Header.Get("Accept-Language"),
		Default:        def,
	}

	if c, err := r.Cookie(CookieName); err == nil {
		p.Cookie = c.Value
	}

	return b.Negotiate(p)
}

// RememberChoice stores an explicit ?lang= choice in the lang cookie so the
// next page keeps the language. It does nothing without a valid ?lang=.
func (b *Bundle) RememberChoice(w http.ResponseWriter, r *http.Request) {
	tag, ok := b.Resolve(r.URL.Query().Get(QueryParam))
	if !ok {
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    tag,
		Path:     "/",
		Expires:  time.Now().Add(365 * 24 * time.Hour),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
}
