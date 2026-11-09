package apis

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #79c): the filter subset and the attribute/PATCH
// parsing of the SCIM endpoints.
//
// Filters: one or more `attr eq value` terms joined by `and`. Attributes
// (case-insensitive): Users: id, userName, externalId, displayName, active,
// emails.value and emails[type eq "work"].value; Groups: id, displayName,
// externalId. Anything else (other operators, or, not, parentheses,
// other attributes) is a 400 invalidFilter.

type scimPred struct {
	attr  string // normalized (see scimNormalizeAttr)
	value any    // string or bool
}

func scimInvalidFilter(msg string) error {
	return &core.SCIMError{Status: 400, Type: "invalidFilter", Detail: msg}
}

var scimEmailsWorkRegex = regexp.MustCompile(`(?i)^emails\[\s*type\s+eq\s+"work"\s*\]\.value$`)

const scimCoreUserPrefix = "urn:ietf:params:scim:schemas:core:2.0:user:"
const scimCoreGroupPrefix = "urn:ietf:params:scim:schemas:core:2.0:group:"

func scimNormalizeAttr(attr string) string {
	low := strings.ToLower(strings.TrimSpace(attr))
	low = strings.TrimPrefix(low, scimCoreUserPrefix)
	low = strings.TrimPrefix(low, scimCoreGroupPrefix)

	if scimEmailsWorkRegex.MatchString(low) {
		return "emails.value"
	}

	return low
}

func parseSCIMFilter(filter string, allowed ...string) ([]scimPred, error) {
	var preds []scimPred

	rest := strings.TrimSpace(filter)
	if rest == "" {
		return nil, nil
	}

	for {
		attr, after, err := scimReadAttr(rest)
		if err != nil {
			return nil, err
		}

		attr = scimNormalizeAttr(attr)

		ok := false

		for _, a := range allowed {
			if a == attr {
				ok = true
			}
		}

		if !ok {
			return nil, scimInvalidFilter("The filter attribute is not supported: " + attr)
		}

		op, after := scimReadWord(after)
		if !strings.EqualFold(op, "eq") {
			return nil, scimInvalidFilter("Only the eq operator is supported (and joins terms).")
		}

		val, after, err := scimReadValue(after)
		if err != nil {
			return nil, err
		}

		if _, isBool := val.(bool); attr == "active" != isBool {
			return nil, scimInvalidFilter("The filter value has the wrong type for " + attr + ".")
		}

		preds = append(preds, scimPred{attr: attr, value: val})

		after = strings.TrimSpace(after)
		if after == "" {
			return preds, nil
		}

		kw, after := scimReadWord(after)
		if !strings.EqualFold(kw, "and") {
			return nil, scimInvalidFilter("Only 'and' can join filter terms.")
		}

		rest = strings.TrimSpace(after)
		if rest == "" {
			return nil, scimInvalidFilter("The filter ends after 'and'.")
		}
	}
}

// scimReadAttr reads up to the first whitespace outside brackets and quotes.
func scimReadAttr(s string) (attr, rest string, err error) {
	s = strings.TrimLeft(s, " \t")
	if strings.HasPrefix(s, "(") || strings.HasPrefix(strings.ToLower(s), "not ") {
		return "", "", scimInvalidFilter("Parentheses and 'not' are not supported.")
	}

	depth, inQuote := 0, false

	for i := 0; i < len(s); i++ {
		c := s[i]

		switch {
		case inQuote:
			switch c {
			case '\\':
				i++
			case '"':
				inQuote = false
			}
		case c == '"':
			inQuote = true
		case c == '[':
			depth++
		case c == ']':
			depth--
		case (c == ' ' || c == '\t') && depth == 0:
			return s[:i], s[i:], nil
		}
	}

	return "", "", scimInvalidFilter("The filter is incomplete.")
}

func scimReadWord(s string) (word, rest string) {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i:]
	}

	return s, ""
}

func scimReadValue(s string) (any, string, error) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return nil, "", scimInvalidFilter("The filter is incomplete.")
	}

	if s[0] == '"' {
		for i := 1; i < len(s); i++ {
			if s[i] == '\\' {
				i++

				continue
			}

			if s[i] == '"' {
				var v string
				if err := json.Unmarshal([]byte(s[:i+1]), &v); err != nil {
					return nil, "", scimInvalidFilter("The filter value is not a valid string.")
				}

				return v, s[i+1:], nil
			}
		}

		return nil, "", scimInvalidFilter("The filter has an unterminated string.")
	}

	word, rest := scimReadWord(s)

	switch strings.ToLower(word) {
	case "true":
		return true, rest, nil
	case "false":
		return false, rest, nil
	}

	return nil, "", scimInvalidFilter("The filter value must be a string or a boolean.")
}

// ---- attributes ------------------------------------------------------------

func scimInvalidValue(msg string) error {
	return &core.SCIMError{Status: 400, Type: "invalidValue", Detail: msg}
}

// scimBool accepts true/false and, as Entra ID sends them, "True"/"False".
func scimBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		if b, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(t))); err == nil {
			return b, nil
		}
	}

	return false, scimInvalidValue("active must be a boolean.")
}

func scimString(v any, name string) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case nil:
		return "", nil
	}

	return "", scimInvalidValue(name + " must be a string.")
}

var scimEmailsPathRegex = regexp.MustCompile(`(?i)^emails\[(.+)\](?:\.value)?$`)

// scimApplyUserAttr applies one attribute (a JSON key of a resource, or the
// path of a PATCH operation) to attrs. Unknown attributes, including the
// enterprise extension, are ignored on purpose: identity providers send many
// attributes Claviger has no use for.
func scimApplyUserAttr(a *core.SCIMUserAttrs, path string, value any, remove bool) error {
	norm := scimNormalizeAttr(path)

	if strings.HasPrefix(norm, "urn:") {
		return nil
	}

	setStr := func(dst **string, name string) error {
		if remove {
			empty := ""
			*dst = &empty

			return nil
		}

		s, err := scimString(value, name)
		if err != nil {
			return err
		}

		*dst = &s

		return nil
	}

	switch norm {
	case "active":
		if remove {
			return nil
		}

		b, err := scimBool(value)
		if err != nil {
			return err
		}

		a.Active = &b

		return nil
	case "username":
		if remove {
			return nil
		}

		return setStr(&a.UserName, "userName")
	case "externalid":
		return setStr(&a.ExternalId, "externalId")
	case "displayname":
		return setStr(&a.DisplayName, "displayName")
	case "name.givenname":
		return setStr(&a.GivenName, "name.givenName")
	case "name.familyname":
		return setStr(&a.FamilyName, "name.familyName")
	case "name.formatted":
		return setStr(&a.Formatted, "name.formatted")
	case "name":
		if remove {
			return nil
		}

		obj, ok := value.(map[string]any)
		if !ok {
			return scimInvalidValue("name must be an object.")
		}

		for k, v := range obj {
			if err := scimApplyUserAttr(a, "name."+k, v, false); err != nil {
				return err
			}
		}

		return nil
	case "emails.value":
		if remove {
			return nil
		}

		return setStr(&a.WorkEmail, "emails.value")
	case "emails":
		if remove {
			return nil
		}

		return scimApplyEmails(a, value)
	}

	if m := scimEmailsPathRegex.FindStringSubmatch(norm); m != nil {
		return scimApplyEmailsFiltered(a, m[1], value, remove)
	}

	return nil
}

func scimApplyEmails(a *core.SCIMUserAttrs, value any) error {
	list, ok := value.([]any)
	if !ok {
		return scimInvalidValue("emails must be an array.")
	}

	var pick map[string]any

	rank := func(m map[string]any) int {
		if p, _ := m["primary"].(bool); p {
			return 3
		}

		if t, _ := m["type"].(string); strings.EqualFold(t, "work") {
			return 2
		}

		return 1
	}

	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}

		if pick == nil || rank(m) > rank(pick) {
			pick = m
		}
	}

	if pick == nil {
		return nil
	}

	s, err := scimString(pick["value"], "emails.value")
	if err != nil {
		return err
	}

	a.WorkEmail = &s

	return nil
}

// scimApplyEmailsFiltered handles emails[type eq "work"] (with or without
// .value) and emails[primary eq true]; other email types are ignored.
func scimApplyEmailsFiltered(a *core.SCIMUserAttrs, filter string, value any, remove bool) error {
	f := strings.ToLower(strings.Join(strings.Fields(filter), " "))
	if f != `type eq "work"` && f != "primary eq true" {
		return nil
	}

	if remove {
		return nil
	}

	if obj, ok := value.(map[string]any); ok {
		value = obj["value"]
	}

	s, err := scimString(value, "emails.value")
	if err != nil {
		return err
	}

	a.WorkEmail = &s

	return nil
}

// scimApplyUserObject applies a whole resource (POST, PUT) or a path-less
// PATCH value.
func scimApplyUserObject(a *core.SCIMUserAttrs, obj map[string]any) error {
	for k, v := range obj {
		if k == "schemas" || k == "id" || k == "meta" || k == "groups" || k == "password" {
			continue
		}

		if err := scimApplyUserAttr(a, k, v, false); err != nil {
			return err
		}
	}

	return nil
}

// scimPatchOp is one operation of a PatchOp request.
type scimPatchOp struct {
	Op    string
	Path  string
	Value any
}

func scimParsePatch(body map[string]any) ([]scimPatchOp, error) {
	rawOps, ok := body["Operations"].([]any)
	if !ok {
		// some clients send the lowercase key
		rawOps, ok = body["operations"].([]any)
	}

	if !ok || len(rawOps) == 0 {
		return nil, &core.SCIMError{Status: 400, Type: "invalidSyntax", Detail: "Operations is required."}
	}

	ops := make([]scimPatchOp, 0, len(rawOps))

	for i, it := range rawOps {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, &core.SCIMError{Status: 400, Type: "invalidSyntax", Detail: fmt.Sprintf("Operation %d is not an object.", i)}
		}

		op, _ := m["op"].(string)
		op = strings.ToLower(strings.TrimSpace(op))

		if op != "add" && op != "replace" && op != "remove" {
			return nil, &core.SCIMError{Status: 400, Type: "invalidSyntax", Detail: fmt.Sprintf("Operation %d has an unsupported op.", i)}
		}

		path, _ := m["path"].(string)

		if op == "remove" && strings.TrimSpace(path) == "" {
			return nil, &core.SCIMError{Status: 400, Type: "noTarget", Detail: "A remove operation needs a path."}
		}

		ops = append(ops, scimPatchOp{Op: op, Path: strings.TrimSpace(path), Value: m["value"]})
	}

	return ops, nil
}

func scimApplyUserPatch(a *core.SCIMUserAttrs, ops []scimPatchOp) error {
	for _, op := range ops {
		if op.Path == "" {
			obj, ok := op.Value.(map[string]any)
			if !ok {
				return scimInvalidValue("A path-less operation needs an object value.")
			}

			if err := scimApplyUserObject(a, obj); err != nil {
				return err
			}

			continue
		}

		if err := scimApplyUserAttr(a, op.Path, op.Value, op.Op == "remove"); err != nil {
			return err
		}
	}

	return nil
}
