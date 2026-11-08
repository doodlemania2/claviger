package azurekv

import "testing"

func TestScopeForHost(t *testing.T) {
	for host, want := range map[string]string{
		"v.vault.azure.net":         "https://vault.azure.net/.default",
		"h.managedhsm.azure.net":    "https://managedhsm.azure.net/.default",
		"v.vault.usgovcloudapi.net": "https://vault.usgovcloudapi.net/.default",
		"v.vault.azure.cn":          "https://vault.azure.cn/.default",
		"127.0.0.1:8443":            "https://vault.azure.net/.default",
		"v.vault.azure.net:443":     "https://vault.azure.net/.default",
	} {
		if got := scopeForHost(host); got != want {
			t.Errorf("%s: %s, want %s", host, got, want)
		}
	}
}
