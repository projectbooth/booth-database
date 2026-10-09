package config

import (
	"net/url"
	"testing"
	"time"
)

func setBase(t *testing.T) {
	t.Setenv("BOOTH_DATABASE_ADMIN_HOST", "pg.internal")
	t.Setenv("BOOTH_DATABASE_ADMIN_USER", "booth_admin")
	t.Setenv("BOOTH_DATABASE_ADMIN_PASSWORD", "p@ss:w/rd?&")
}

func TestLoad_BundledDefaults(t *testing.T) {
	setBase(t)
	t.Setenv("BOOTH_DATABASE_RESTRICT_MAINTENANCE_ACCESS", "false") // ignored when bundled
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeBundled || !cfg.RestrictMaintenanceAccess {
		t.Errorf("bundled mode must always restrict maintenance access: %+v", cfg)
	}
	if cfg.MinTTL != time.Hour || cfg.MaxTTL != time.Hour || cfg.ReapInterval != 10*time.Second || cfg.LeaseConnectionLimit != 10 {
		t.Errorf("defaults = min %v max %v reap %v limit %d", cfg.MinTTL, cfg.MaxTTL, cfg.ReapInterval, cfg.LeaseConnectionLimit)
	}
	if cfg.ClientHost != "pg.internal" || cfg.ClientPort != 5432 || cfg.ClientSSLMode != "prefer" {
		t.Errorf("client endpoint should default to the admin one: %s:%d %s", cfg.ClientHost, cfg.ClientPort, cfg.ClientSSLMode)
	}
	u, err := url.Parse(cfg.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw != "p@ss:w/rd?&" || u.Path != "/postgres" {
		t.Errorf("admin DSN didn't round-trip a password with URL metacharacters: %v", u)
	}
}

func TestLoad_External(t *testing.T) {
	setBase(t)
	t.Setenv("BOOTH_DATABASE_MODE", "external")
	t.Setenv("BOOTH_DATABASE_CLIENT_HOST", "pg.public.example")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RestrictMaintenanceAccess {
		t.Error("external mode must default restrictMaintenanceAccess to false (ADR 0054 §2)")
	}
	if cfg.ClientHost != "pg.public.example" {
		t.Errorf("ClientHost = %q", cfg.ClientHost)
	}
	t.Setenv("BOOTH_DATABASE_RESTRICT_MAINTENANCE_ACCESS", "true")
	if cfg, _ = Load(); !cfg.RestrictMaintenanceAccess {
		t.Error("external mode should honour an explicit opt-in")
	}
}

func TestLoad_Errors(t *testing.T) {
	t.Run("missing admin", func(t *testing.T) {
		if _, err := Load(); err == nil {
			t.Error("loaded without admin credentials")
		}
	})
	for key, val := range map[string]string{
		"BOOTH_DATABASE_MODE":                   "sideways",
		"BOOTH_DATABASE_MAX_TTL":                "forever",
		"BOOTH_DATABASE_REAP_INTERVAL":          "0s",
		"BOOTH_DATABASE_LEASE_CONNECTION_LIMIT": "-1",
		"BOOTH_DATABASE_ADMIN_PORT":             "abc",
	} {
		t.Run(key, func(t *testing.T) {
			setBase(t)
			t.Setenv(key, val)
			if _, err := Load(); err == nil {
				t.Errorf("accepted %s=%s", key, val)
			}
		})
	}
}

func TestLoad_NodePin(t *testing.T) {
	setBase(t)
	t.Setenv("BOOTH_DATABASE_PIN_STATEFULSET", "db-booth-database-postgres")
	if _, err := Load(); err == nil {
		t.Error("pinning without BOOTH_NAMESPACE accepted")
	}
	t.Setenv("BOOTH_NAMESPACE", "booth-database")
	if cfg, err := Load(); err != nil || cfg.PinStatefulSet != "db-booth-database-postgres" {
		t.Errorf("cfg=%+v err=%v", cfg, err)
	}
	t.Setenv("BOOTH_DATABASE_MODE", "external")
	if _, err := Load(); err == nil {
		t.Error("pinning accepted in external mode")
	}
}

func TestLoad_TTLWindow(t *testing.T) {
	setBase(t)
	t.Setenv("BOOTH_DATABASE_MIN_TTL", "10m")
	cfg, err := Load()
	if err != nil || cfg.MinTTL != 10*time.Minute || cfg.MaxTTL != 10*time.Minute {
		t.Fatalf("MaxTTL should default to the floor: %+v %v", cfg, err)
	}
	t.Setenv("BOOTH_DATABASE_MAX_TTL", "2h")
	if cfg, err = Load(); err != nil || cfg.MaxTTL != 2*time.Hour {
		t.Fatalf("explicit cap: %+v %v", cfg, err)
	}
	t.Setenv("BOOTH_DATABASE_MAX_TTL", "5m")
	if _, err := Load(); err == nil {
		t.Error("accepted a cap below the floor")
	}
	t.Setenv("BOOTH_DATABASE_MAX_TTL", "")
	t.Setenv("BOOTH_DATABASE_MIN_TTL", "0s")
	if _, err := Load(); err == nil {
		t.Error("accepted a zero floor")
	}
}

func TestLoad_AdminView(t *testing.T) {
	setBase(t)
	cfg, err := Load()
	if err != nil || cfg.OIDC.IssuerURL != "" {
		t.Fatalf("defaults: admin API off: %+v %v", cfg, err)
	}
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp.example/realms/booth")
	if _, err := Load(); err == nil {
		t.Error("issuer without a client id accepted")
	}
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "booth-database")
	cfg, err = Load()
	if err != nil || cfg.OIDC.GroupsClaim != "groups" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	// ADR 0094: the old allowlist is refused loudly, never silently ignored.
	t.Setenv("BOOTH_DATABASE_OPERATOR_WORKSPACES", "platform")
	if _, err := Load(); err == nil {
		t.Error("the retired operator-workspace allowlist was accepted")
	}
}

// ADR 0108's key-fetch override.
func TestLoad_JWKSURL(t *testing.T) {
	setBase(t)
	if cfg, err := Load(); err != nil || cfg.OIDC.JWKSURL != "" {
		t.Fatalf("default must be empty (discovery): %+v %v", cfg.OIDC, err)
	}
	t.Setenv("BOOTH_OIDC_JWKS_URL", "http://keycloak.booth-system.svc:8080/realms/booth/protocol/openid-connect/certs")
	if _, err := Load(); err == nil {
		t.Error("BOOTH_OIDC_JWKS_URL without BOOTH_OIDC_ISSUER_URL must be a startup error")
	}
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://booth.example/realms/booth")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "booth-database")
	cfg, err := Load()
	if err != nil || cfg.OIDC.JWKSURL != "http://keycloak.booth-system.svc:8080/realms/booth/protocol/openid-connect/certs" || cfg.OIDC.IssuerURL != "https://booth.example/realms/booth" {
		t.Fatalf("cfg=%+v err=%v", cfg.OIDC, err)
	}
}
