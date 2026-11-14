package apis_test

import (
	"net/http"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #87, 87-5): the read routes of the register screens.

func TestRegisterApiListAndEntries(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 2)

	final := env.draft("Final")
	pending := env.draft("Pending")
	env.draft("Draft")

	// a draft of another org
	col, _ := env.app.FindCollectionByNameOrId("baptisms")
	other := core.NewRecord(col)
	other.Set("org", orgP2)
	other.Set("sacrament", "baptism")
	other.Set("surname", "Elsewhere")
	orgMust(t, env.app.Save(other))

	// two signers: carol asks, frank confirms; a second entry stays pending
	env.finalize(env.fresh("carol"), final)
	env.finalize(env.fresh("frank"), final)
	env.finalize(env.fresh("carol"), pending)

	if status, _ := env.do(http.MethodGet, "/api/registers?org="+orgP1, nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}

	// the register list, with what the caller may do
	status, out := env.do(http.MethodGet, "/api/registers?org="+orgP1, nil, env.plain("carol"))
	items, _ := out["items"].([]any)
	if status != http.StatusOK || len(items) != 1 || out["registrar"] != true || out["superuser"] != false {
		t.Fatalf("list: %d %v", status, out)
	}

	first := items[0].(map[string]any)
	if first["collection"] != "baptisms" || first["signers"] != float64(2) || first["chain"] != true {
		t.Fatalf("summary: %v", first)
	}

	names := map[string]bool{}
	for _, f := range first["fields"].([]any) {
		names[f.(map[string]any)["name"].(string)] = true
	}

	if !names["surname"] || names["hash"] || names["pendingBy"] || names["org"] {
		t.Fatalf("display fields: %v", names)
	}

	if _, out = env.do(http.MethodGet, "/api/registers?org="+orgP1, nil, env.plain("bob")); out["registrar"] != false {
		t.Fatalf("an admin is not a registrar: %v", out)
	}

	for who, want := range map[string]int{"dave": 403, "erin": 404} {
		if status, _ := env.do(http.MethodGet, "/api/registers?org="+orgP1, nil, env.plain(who)); status != want {
			t.Errorf("%s list: want %d got %d", who, want, status)
		}
	}

	if status, _ := env.do(http.MethodGet, "/api/registers", nil, env.plain("carol")); status != http.StatusBadRequest {
		t.Fatalf("no org: %d", status)
	}

	if status, out = env.do(http.MethodGet, "/api/registers", nil, auditSuperuserToken); status != http.StatusOK || out["superuser"] != true {
		t.Fatalf("superuser list: %d %v", status, out)
	}

	// entries of the org only, by state
	count := func(token, query string) (int, int) {
		status, out := env.do(http.MethodGet, "/api/registers/baptisms"+query, nil, token)
		list, _ := out["items"].([]any)

		return status, len(list)
	}

	for _, c := range []struct {
		query string
		want  int
	}{
		{"?org=" + orgP1, 3}, {"?org=" + orgP1 + "&state=final", 1}, {"?org=" + orgP1 + "&state=pending", 1}, {"?org=" + orgP1 + "&state=draft", 1},
	} {
		if status, n := count(env.plain("bob"), c.query); status != http.StatusOK || n != c.want {
			t.Errorf("%s: %d entries (%d), want %d", c.query, n, status, c.want)
		}
	}

	if status, out := env.do(http.MethodGet, "/api/registers/baptisms?org="+orgP1+"&perPage=2&page=2", nil, env.plain("bob")); status != http.StatusOK ||
		out["totalItems"] != float64(3) || len(out["items"].([]any)) != 1 {
		t.Fatalf("paging: %d %v", status, out)
	}

	// the superuser reads across orgs; a member of P1 never sees P2's entries
	if _, n := count(auditSuperuserToken, ""); n != 4 {
		t.Errorf("superuser sees %d entries, want 4", n)
	}

	if status, _ := count(env.plain("erin"), "?org="+orgP1); status != http.StatusNotFound {
		t.Errorf("a stranger: %d", status)
	}

	if status, _ := count(env.plain("dave"), "?org="+orgP1); status != http.StatusForbidden {
		t.Errorf("a viewer: %d", status)
	}

	if status, _ := count(env.plain("bob"), "?org="+orgP1+"&state=nope"); status != http.StatusBadRequest {
		t.Errorf("a bad state: %d", status)
	}

	if status, _ := count(env.plain("bob"), "?org="+orgP2); status != http.StatusNotFound && status != http.StatusForbidden {
		t.Errorf("another org: %d", status)
	}

	if status, _ := env.do(http.MethodGet, "/api/registers/nope?org="+orgP1, nil, env.plain("bob")); status != http.StatusNotFound {
		t.Errorf("an unknown register: %d", status)
	}
}

func TestRegisterApiVerify(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.draft("Smith")
	env.finalize(env.fresh("carol"), id)

	if status, _ := env.do(http.MethodGet, "/api/registers/_verify", nil, env.plain("carol")); status != http.StatusForbidden {
		t.Fatalf("a registrar: %d", status)
	}

	status, out := env.do(http.MethodGet, "/api/registers/_verify", nil, auditSuperuserToken)
	regs, _ := out["registers"].([]any)

	if status != http.StatusOK || out["ok"] != true || len(regs) != 1 {
		t.Fatalf("verify: %d %v", status, out)
	}

	v := regs[0].(map[string]any)
	if v["collection"] != "baptisms" || v["finalized"] != float64(1) || v["headSeq"] != float64(1) || v["headHash"] == "" {
		t.Fatalf("result: %v", v)
	}

	// a change outside the application is reported
	if _, err := env.app.NonconcurrentDB().NewQuery("UPDATE baptisms SET surname = 'Tampered' WHERE id = {:id}").Bind(map[string]any{"id": id}).Execute(); err != nil {
		t.Fatal(err)
	}

	status, out = env.do(http.MethodGet, "/api/registers/_verify", nil, auditSuperuserToken)
	if status != http.StatusOK || out["ok"] != false {
		t.Fatalf("after tampering: %d %v", status, out)
	}
}
