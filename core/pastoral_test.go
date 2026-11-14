package core_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #86): the rule builder and the shape marker. The rules
// run against real collections in apis/pastoral_test.go.

func TestPastoralRulesNameNoOwnerOrAdminClause(t *testing.T) {
	r := core.PastoralRules("pastoral_notes")

	for name, rule := range map[string]*string{"list": r.List, "view": r.View, "create": r.Create, "update": r.Update, "delete": r.Delete} {
		if rule == nil || *rule == "" {
			t.Fatalf("%s rule must be set", name)
		}

		for _, bad := range []string{`role ?= "owner"`, `role ?= "admin"`, "parent", "orgClosure"} {
			if strings.Contains(*rule, bad) {
				t.Fatalf("%s rule must not contain %q: %s", name, bad, *rule)
			}
		}
	}

	if !strings.Contains(*r.List, "@collection._breakGlass:g.approvedBy") || !strings.Contains(*r.List, "activeFrom ?<= @now") {
		t.Fatalf("the read rule needs the break-glass clause: %s", *r.List)
	}

	if len(core.PastoralRuleProblems("pastoral_notes", r)) != 0 {
		t.Fatal("the locked rules must pass their own check")
	}

	loose := `@request.auth.id != ""`
	if p := core.PastoralRuleProblems("pastoral_notes", core.OrgRuleSet{List: &loose}); len(p) != 1 {
		t.Fatalf("a looser list rule is one problem: %v", p)
	}

	if p := core.PastoralRuleProblems("pastoral_notes", core.OrgRuleSet{}); len(p) != 0 {
		t.Fatalf("null rules are stricter: %v", p)
	}
}
