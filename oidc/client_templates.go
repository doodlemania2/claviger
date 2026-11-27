package oidc

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/doodlemania2/claviger/core"
)

//go:embed templates/*.json
var templateFS embed.FS

// TemplatePlaceholder is a value the admin fills in before a template becomes a
// client (the product's tenant URL, for example).
type TemplatePlaceholder struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Hint    string `json:"hint"`
	Example string `json:"example"`
}

// TemplateClaim is a claim that leaves for the third party when the scope is granted.
type TemplateClaim struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Description string `json:"description"`
}

// ClientTemplate is a preset for an OAuth client of a third-party product.
// Redirect URI patterns hold {placeholders}.
type ClientTemplate struct {
	ID              string                `json:"id"`
	Name            string                `json:"name"`
	Vendor          string                `json:"vendor"`
	Description     string                `json:"description"`
	Verified        bool                  `json:"verified"`
	Guide           string                `json:"guide"`
	Placeholders    []TemplatePlaceholder `json:"placeholders"`
	RedirectURIs    []string              `json:"redirectUris"`
	PostLogoutURIs  []string              `json:"postLogoutUris"`
	Scopes          []string              `json:"scopes"`
	Public          bool                  `json:"public"`
	FirstParty      bool                  `json:"firstParty"`
	RequirePKCE     bool                  `json:"requirePkce"`
	AccessTokenType string                `json:"accessTokenType"`
	SigningAlg      string                `json:"signingAlg"`
	IDTokenSeconds  int                   `json:"idTokenSeconds"`
	Claims          []TemplateClaim       `json:"claims"`
}

var placeholderRe = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

var (
	templatesOnce sync.Once
	templatesList []ClientTemplate
	templatesErr  error
)

// ClientTemplates returns the templates embedded in the binary, generic last.
func ClientTemplates() ([]ClientTemplate, error) {
	templatesOnce.Do(func() {
		templatesList, templatesErr = loadTemplates(templateFS, "templates")
	})

	return templatesList, templatesErr
}

func loadTemplates(fsys fs.FS, dir string) ([]ClientTemplate, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}

	var out []ClientTemplate

	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}

		raw, err := fs.ReadFile(fsys, path.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}

		var tpl ClientTemplate
		if err := json.Unmarshal(raw, &tpl); err != nil {
			return nil, fmt.Errorf("%s: %w", ent.Name(), err)
		}

		if err := tpl.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", ent.Name(), err)
		}

		out = append(out, tpl)
	}

	sort.SliceStable(out, func(i, j int) bool {
		gi, gj := out[i].ID == "generic", out[j].ID == "generic"
		if gi != gj {
			return gj
		}

		return out[i].Name < out[j].Name
	})

	return out, nil
}

func (t ClientTemplate) validate() error {
	if t.ID == "" || t.Name == "" || len(t.RedirectURIs) == 0 || len(t.Scopes) == 0 {
		return fmt.Errorf("id, name, redirectUris and scopes are required")
	}

	declared := map[string]bool{}
	for _, p := range t.Placeholders {
		declared[p.Key] = true
	}

	for _, uri := range append(append([]string{}, t.RedirectURIs...), t.PostLogoutURIs...) {
		for _, m := range placeholderRe.FindAllStringSubmatch(uri, -1) {
			if !declared[m[1]] {
				return fmt.Errorf("placeholder {%s} in %q is not declared", m[1], uri)
			}
		}
	}

	scopes := map[string]bool{}
	for _, s := range t.Scopes {
		scopes[s] = true
	}

	for _, c := range t.Claims {
		if !scopes[c.Scope] {
			return fmt.Errorf("claim %q needs scope %q, which the template does not request", c.Name, c.Scope)
		}
	}

	return nil
}

func (m *module) listClientTemplates(e *core.RequestEvent) error {
	tpls, err := ClientTemplates()
	if err != nil {
		return e.InternalServerError("Failed to load the client templates.", err)
	}

	return e.JSON(http.StatusOK, tpls)
}
