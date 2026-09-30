// Package config loads booth-database's runtime configuration from environment variables. Every
// value maps 1:1 to a Helm chart value/env var, mirroring booth-storage's and booth-core's own
// internal/config — there is no config file format of our own to version.
package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"
)

const (
	ModeBundled  = "bundled"
	ModeExternal = "external"
)

// Config is booth-database's full runtime configuration.
type Config struct {
	HTTPAddr string

	// Mode is "bundled" (this chart's own StatefulSet) or "external" (an operator-supplied
	// cluster) — ADR 0081's admin choice, mirroring ADR 0053/0054's shape for core's Postgres.
	Mode string

	// AdminDSN is this module's own admin connection (a maintenance database, as a role with
	// CREATEDB and CREATEROLE). Assembled from separate fields so the password can come from a
	// Secret on its own; never handed to anyone.
	AdminDSN string

	// ClientHost/ClientPort/ClientSSLMode are what an issued credential tells a requester to
	// connect to. Default to the admin connection's own, but can differ — e.g. an external
	// cluster reached through a different address from workload pods than from this module.
	ClientHost    string
	ClientPort    int
	ClientSSLMode string

	// RestrictMaintenanceAccess revokes PUBLIC CONNECT on postgres/template1 at startup. Always
	// true when bundled; opt-in (default false, with a startup warning) when external — ADR
	// 0054 §2's exact precedent.
	RestrictMaintenanceAccess bool

	// MinTTL is the provider's floor on a lease's lifetime (ADR 0089 §3): every request below it —
	// under the broker's 5-minute ceiling, every request — is clamped up to it. Default one hour;
	// reasoning in docs/decisions/0003.
	MinTTL time.Duration
	// MaxTTL caps a lease's lifetime on this side, independently of the broker's own ceiling.
	// Must be >= MinTTL; defaults to it.
	MaxTTL time.Duration
	// LeaseConnectionLimit is each lease role's CONNECTION LIMIT.
	LeaseConnectionLimit int
	// ReapInterval is how often expired leases' open sessions are terminated and roles dropped —
	// i.e. the worst-case time a session opened before expiry can outlive it.
	ReapInterval time.Duration

	// CredentialBrokerCredential is this module's copy of the secret booth-core presents on
	// POST /internal/credentials (the booth-credential-broker-provider-credentials Secret).
	// Empty refuses every broker call.
	CredentialBrokerCredential string

	// PinStatefulSet names the bundled PostgreSQL StatefulSet to pin to its node (ADR 0090,
	// internal/nodepin), in Namespace. Empty disables pinning: external mode, or an operator who
	// turned bundled.pinToNode off.
	PinStatefulSet string
	Namespace      string
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:                   getEnv("BOOTH_HTTP_ADDR", ":8080"),
		Mode:                       getEnv("BOOTH_DATABASE_MODE", ModeBundled),
		CredentialBrokerCredential: os.Getenv("BOOTH_CREDENTIAL_BROKER_CREDENTIAL"),
	}
	if cfg.Mode != ModeBundled && cfg.Mode != ModeExternal {
		return Config{}, fmt.Errorf("BOOTH_DATABASE_MODE must be %q or %q, got %q", ModeBundled, ModeExternal, cfg.Mode)
	}

	host := os.Getenv("BOOTH_DATABASE_ADMIN_HOST")
	user := os.Getenv("BOOTH_DATABASE_ADMIN_USER")
	password := os.Getenv("BOOTH_DATABASE_ADMIN_PASSWORD")
	if host == "" || user == "" || password == "" {
		return Config{}, fmt.Errorf("BOOTH_DATABASE_ADMIN_HOST, BOOTH_DATABASE_ADMIN_USER and BOOTH_DATABASE_ADMIN_PASSWORD are required")
	}
	port, err := intEnv("BOOTH_DATABASE_ADMIN_PORT", 5432)
	if err != nil {
		return Config{}, err
	}
	sslmode := getEnv("BOOTH_DATABASE_ADMIN_SSLMODE", "prefer")
	adminURL := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     fmt.Sprintf("%s:%d", host, port),
		Path:     "/" + getEnv("BOOTH_DATABASE_ADMIN_DBNAME", "postgres"),
		RawQuery: url.Values{"sslmode": {sslmode}, "application_name": {"booth-database"}}.Encode(),
	}
	cfg.AdminDSN = adminURL.String()

	cfg.ClientHost = getEnv("BOOTH_DATABASE_CLIENT_HOST", host)
	if cfg.ClientPort, err = intEnv("BOOTH_DATABASE_CLIENT_PORT", port); err != nil {
		return Config{}, err
	}
	cfg.ClientSSLMode = getEnv("BOOTH_DATABASE_CLIENT_SSLMODE", sslmode)

	switch v := os.Getenv("BOOTH_DATABASE_RESTRICT_MAINTENANCE_ACCESS"); {
	case cfg.Mode == ModeBundled:
		cfg.RestrictMaintenanceAccess = true
	case v == "":
		cfg.RestrictMaintenanceAccess = false
	default:
		if cfg.RestrictMaintenanceAccess, err = strconv.ParseBool(v); err != nil {
			return Config{}, fmt.Errorf("BOOTH_DATABASE_RESTRICT_MAINTENANCE_ACCESS: %w", err)
		}
	}

	if cfg.MinTTL, err = durationEnv("BOOTH_DATABASE_MIN_TTL", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.MaxTTL, err = durationEnv("BOOTH_DATABASE_MAX_TTL", cfg.MinTTL); err != nil {
		return Config{}, err
	}
	if cfg.MinTTL <= 0 {
		return Config{}, fmt.Errorf("BOOTH_DATABASE_MIN_TTL must be positive")
	}
	if cfg.MaxTTL < cfg.MinTTL {
		return Config{}, fmt.Errorf("BOOTH_DATABASE_MAX_TTL (%s) must be at least BOOTH_DATABASE_MIN_TTL (%s)", cfg.MaxTTL, cfg.MinTTL)
	}
	if cfg.ReapInterval, err = durationEnv("BOOTH_DATABASE_REAP_INTERVAL", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.LeaseConnectionLimit, err = intEnv("BOOTH_DATABASE_LEASE_CONNECTION_LIMIT", 10); err != nil {
		return Config{}, err
	}
	// MaxTTL needs no check of its own here: it was already required to be >= a positive MinTTL.
	if cfg.ReapInterval <= 0 || cfg.LeaseConnectionLimit <= 0 {
		return Config{}, fmt.Errorf("BOOTH_DATABASE_REAP_INTERVAL and BOOTH_DATABASE_LEASE_CONNECTION_LIMIT must be positive")
	}

	cfg.PinStatefulSet = os.Getenv("BOOTH_DATABASE_PIN_STATEFULSET")
	cfg.Namespace = os.Getenv("BOOTH_NAMESPACE")
	if cfg.PinStatefulSet != "" && cfg.Mode != ModeBundled {
		return Config{}, fmt.Errorf("BOOTH_DATABASE_PIN_STATEFULSET only applies to bundled mode")
	}
	if cfg.PinStatefulSet != "" && cfg.Namespace == "" {
		return Config{}, fmt.Errorf("BOOTH_NAMESPACE is required when BOOTH_DATABASE_PIN_STATEFULSET is set")
	}

	if cfg.CredentialBrokerCredential == "" {
		log.Print("WARNING: BOOTH_CREDENTIAL_BROKER_CREDENTIAL is empty — every credential-broker call will be refused. booth-core writes it once this module's manifest declares providesCredentials.")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, v)
	}
	return n, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration like 5m, got %q", key, v)
	}
	return d, nil
}
