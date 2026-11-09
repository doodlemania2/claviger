// Package i18n is Claviger's small message catalog (fork-local, #97).
//
// A catalog is a flat JSON object per locale, embedded in the binary:
//
//	{
//	  "_name": "Español",
//	  "_status": "draft",
//	  "common.signIn": "Iniciar sesión",
//	  "demo.items.one": "{count} elemento",
//	  "demo.items.other": "{count} elementos"
//	}
//
// Keys starting with "_" are metadata. "{name}" placeholders are replaced by
// the arguments of [Bundle.T]; unknown placeholders are left as they are (the
// email templates rely on that for "{APP_NAME}" and friends). A key may carry
// CLDR plural forms as "key.zero|one|two|few|many|other", see [Bundle.Plural].
//
// Lookups fall back along a chain, for example pt-BR, then pt, then the
// default language (en), so a locale file may be incomplete.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed locales/*.json
var embedded embed.FS

// FallbackLanguage is the language every chain ends in.
const FallbackLanguage = "en"

// Status values of the "_status" metadata key.
const (
	StatusReviewed = "reviewed"
	StatusDraft    = "draft"
)

// Locale describes one available language.
type Locale struct {
	Tag    string // canonical tag, for example "pt-BR"
	Name   string // the name of the language in itself
	Status string // "reviewed" or "draft" (machine-quality)
	Dir    string // "ltr" or "rtl"
}

// Bundle holds the messages of every loaded language. It is safe for
// concurrent use.
type Bundle struct {
	mu   sync.RWMutex
	msgs map[string]map[string]string // canonical tag -> key -> message
	meta map[string]Locale
}

// New returns an empty bundle.
func New() *Bundle {
	return &Bundle{msgs: map[string]map[string]string{}, meta: map[string]Locale{}}
}

var (
	defaultOnce   sync.Once
	defaultBundle *Bundle
)

// Default returns the bundle of the catalogs embedded in the binary. It must
// not be modified; use [Bundle.Clone] for a bundle that takes overrides.
func Default() *Bundle {
	defaultOnce.Do(func() {
		b := New()
		if err := b.LoadFS(embedded, "locales"); err != nil {
			panic("i18n: invalid embedded catalog: " + err.Error())
		}

		defaultBundle = b
	})

	return defaultBundle
}

// Clone returns an independent copy.
func (b *Bundle) Clone() *Bundle {
	b.mu.RLock()
	defer b.mu.RUnlock()

	c := New()
	for tag, m := range b.msgs {
		c.msgs[tag] = maps(m)
	}

	for tag, l := range b.meta {
		c.meta[tag] = l
	}

	return c
}

func maps(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}

	return out
}

// LoadFS loads every "<tag>.json" file of dir. A file for a tag that is
// already loaded overrides its messages key by key.
func (b *Bundle) LoadFS(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}

	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}

		raw, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return err
		}

		if err := b.LoadJSON(strings.TrimSuffix(name, ".json"), raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	return nil
}

// LoadDirOS is LoadFS for a directory of the operating system. A missing
// directory is not an error.
func (b *Bundle) LoadDirOS(dir string) error {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	return b.LoadFS(os.DirFS(dir), ".")
}

// LoadJSON merges one catalog file into the bundle.
func (b *Bundle) LoadJSON(tag string, raw []byte) error {
	tag = Canonical(tag)
	if tag == "" {
		return fmt.Errorf("invalid language tag")
	}

	var entries map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}

	msgs := make(map[string]string, len(entries))
	meta := map[string]string{}

	for k, v := range entries {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q: the value must be a string", k)
		}

		if strings.HasPrefix(k, "_") {
			meta[k] = s
		} else {
			msgs[k] = s
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.msgs[tag] == nil {
		b.msgs[tag] = map[string]string{}
	}

	for k, v := range msgs {
		b.msgs[tag][k] = v
	}

	l := b.meta[tag]
	l.Tag = tag
	l.Dir = Dir(tag)

	if v := meta["_name"]; v != "" {
		l.Name = v
	}

	if v := meta["_status"]; v != "" {
		l.Status = v
	}

	if l.Name == "" {
		l.Name = tag
	}

	if l.Status == "" {
		l.Status = StatusReviewed
	}

	b.meta[tag] = l

	return nil
}

// Override sets messages of a language, adding the language when it is new.
func (b *Bundle) Override(tag string, msgs map[string]string) {
	tag = Canonical(tag)
	if tag == "" {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.msgs[tag] == nil {
		b.msgs[tag] = map[string]string{}
		b.meta[tag] = Locale{Tag: tag, Name: tag, Status: StatusReviewed, Dir: Dir(tag)}
	}

	for k, v := range msgs {
		b.msgs[tag][k] = v
	}
}

// Locales lists the available languages sorted by tag.
func (b *Bundle) Locales() []Locale {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]Locale, 0, len(b.meta))
	for _, l := range b.meta {
		out = append(out, l)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })

	return out
}

// Messages returns a copy of the messages of exactly one language (no fallback).
func (b *Bundle) Messages(tag string) map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return maps(b.msgs[tag])
}

// Chain returns the lookup order for a tag: the tag itself, its primary
// language and the fallback language, keeping only the loaded ones.
func (b *Bundle) Chain(tag string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.chain(tag)
}

func (b *Bundle) chain(tag string) []string {
	var out []string

	add := func(t string) {
		if _, ok := b.msgs[t]; ok && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}

	tag = Canonical(tag)
	add(tag)

	// pt-BR -> pt, zh-Hans-CN -> zh-Hans -> zh
	parts := strings.Split(tag, "-")
	for i := len(parts) - 1; i >= 1; i-- {
		add(strings.Join(parts[:i], "-"))
	}

	add(FallbackLanguage)

	return out
}

// Lookup finds a message along the fallback chain.
func (b *Bundle) Lookup(tag, key string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.lookup(tag, key)
}

func (b *Bundle) lookup(tag, key string) (string, bool) {
	for _, t := range b.chain(tag) {
		if v, ok := b.msgs[t][key]; ok {
			return v, true
		}
	}

	return "", false
}

// Args are the values of the {placeholders} of a message.
type Args map[string]any

// T returns the message for key in the language (falling back along the
// chain), with the placeholders replaced. A key that exists nowhere is
// returned as it is, so a missing message is visible and never empty.
func (b *Bundle) T(tag, key string, args ...Args) string {
	msg, ok := b.Lookup(tag, key)
	if !ok {
		return key
	}

	return Format(msg, args...)
}

// Raw is T without placeholder replacement.
func (b *Bundle) Raw(tag, key string) string {
	if msg, ok := b.Lookup(tag, key); ok {
		return msg
	}

	return key
}

// Plural picks the CLDR plural form of key for n ("key.one", "key.other", ...)
// in the language and sets the {count} placeholder to n.
func (b *Bundle) Plural(tag, key string, n int, args ...Args) string {
	all := append([]Args{{"count": n}}, args...)

	b.mu.RLock()
	defer b.mu.RUnlock()

	// the plural rules are those of the language that has the message
	for _, t := range b.chain(tag) {
		if msg, ok := b.msgs[t][key+"."+PluralCategory(tag, n)]; ok {
			return Format(msg, all...)
		}

		if msg, ok := b.msgs[t][key+".other"]; ok {
			return Format(msg, all...)
		}
	}

	return key
}

// Subset returns the messages whose key starts with prefix, resolved along the
// chain of the language, placeholders untouched. It is what the hosted pages
// hand to their scripts.
func (b *Bundle) Subset(tag, prefix string) map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := map[string]string{}

	chain := b.chain(tag)
	for i := len(chain) - 1; i >= 0; i-- { // fallbacks first, the language itself wins
		for k, v := range b.msgs[chain[i]] {
			if strings.HasPrefix(k, prefix) {
				out[k] = v
			}
		}
	}

	return out
}

// Format replaces "{name}" placeholders with the args (fmt "%v"); a
// placeholder without an argument stays as it is.
func Format(msg string, args ...Args) string {
	if len(args) == 0 || !strings.Contains(msg, "{") {
		return msg
	}

	var sb strings.Builder

	for i := 0; i < len(msg); {
		if msg[i] == '{' {
			if end := strings.IndexByte(msg[i:], '}'); end > 1 {
				name := msg[i+1 : i+end]
				if v, ok := lookupArg(name, args); ok {
					sb.WriteString(toString(v))

					i += end + 1

					continue
				}
			}
		}

		sb.WriteByte(msg[i])
		i++
	}

	return sb.String()
}

func lookupArg(name string, args []Args) (any, bool) {
	for i := len(args) - 1; i >= 0; i-- {
		if v, ok := args[i][name]; ok {
			return v, true
		}
	}

	return nil, false
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

// Placeholders returns the sorted, de-duplicated {placeholder} names of a message.
func Placeholders(msg string) []string {
	var out []string

	for i := 0; i < len(msg); i++ {
		if msg[i] != '{' {
			continue
		}

		end := strings.IndexByte(msg[i:], '}')
		if end < 2 {
			continue
		}

		name := msg[i+1 : i+end]
		if !strings.ContainsAny(name, " \t\n{\"'") && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}
