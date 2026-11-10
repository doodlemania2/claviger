package apis

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #84, 84-3 and 84-5): hierarchy helpers for the admin UI
// and the counts-only status route. See bindOrgsApi for the route list.

// orgsConfig returns the effective depth settings (read-only, they come from
// the environment) and the kind labels in use.
func orgsConfig(e *core.RequestEvent) error {
	return e.JSON(http.StatusOK, map[string]any{
		"maxDepth":     core.OrgMaxDepth(),
		"defaultDepth": core.OrgDefaultMaxDepth,
		"hardMaxDepth": core.OrgHardMaxDepth,
		"envVar":       core.OrgMaxDepthEnv,
		"kinds":        core.OrgKinds(e.App),
	})
}

// orgAncestorRule renders core.OrgAncestorRule for the rule snippets helper:
// ?field=org&roles=admin,owner&maxDepth=2&tag=safeguarding. The text is the
// output of the same Go function that tests and applications use, so the UI
// never mirrors it.
func orgAncestorRule(e *core.RequestEvent) error {
	q := e.Request.URL.Query()

	field := strings.TrimSpace(q.Get("field"))
	if field == "" {
		field = "org"
	}

	opts := core.OrgAncestorOpts{Tag: strings.TrimSpace(q.Get("tag"))}

	for _, r := range strings.Split(q.Get("roles"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			opts.Roles = append(opts.Roles, r)
		}
	}

	if v := strings.TrimSpace(q.Get("maxDepth")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return e.BadRequestError("The maximum depth must be a number.", nil)
		}

		opts.MaxDepth = n
	}

	rule, err := core.OrgAncestorRule(field, opts)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	set, err := core.OrgOversightRuleSet(field, opts)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return e.JSON(http.StatusOK, map[string]any{"field": field, "rule": rule, "ruleSet": set})
}

// orgStatus answers the counts-only status of an org to superusers and to
// its owners and admins.
func orgStatus(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	if !actor.CanManage() {
		return e.ForbiddenError("Only owners and admins can see the status of the organization.", nil)
	}

	report, err := core.OrgStatus(e.App, org)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, report)
}
