package apis

import "testing"

// Fork-local: routes with their own bearer scheme are not rejected as unknown record tokens.
func TestCarriesOwnBearer(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]bool{
		"/scim/v2/Users":             true,
		"/api/metrics":               true,
		"/oidc/userinfo":             true,
		"/oidc/oauth/introspect":     true,
		"/api/collections":           false,
		"/api/collections/x/records": false,
		"/scim/v2x":                  false,
		"/api/metricsx":              false,
	} {
		if got := carriesOwnBearer(path); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
}
