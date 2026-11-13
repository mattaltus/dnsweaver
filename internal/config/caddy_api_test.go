package config

import "testing"

func TestCaddyAPIURL_FromEnv(t *testing.T) {
	t.Setenv("DNSWEAVER_CADDY_API_URL", "http://caddy:2019")

	cfg, errs := loadGlobalConfig()
	if len(errs) != 0 {
		t.Fatalf("loadGlobalConfig() errors = %v", errs)
	}
	if cfg.CaddyAPIURL != "http://caddy:2019" {
		t.Errorf("CaddyAPIURL = %q, want %q", cfg.CaddyAPIURL, "http://caddy:2019")
	}
}
