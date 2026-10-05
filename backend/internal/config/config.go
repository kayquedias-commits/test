// Package config provides environment-driven runtime configuration for the
// Financial Management System. All runtime knobs (database, Redis, AMQP,
// object store, TLS, session/audit retention, accounting rules) are sourced
// from the environment so secrets stay in Vault or k8s Secrets. Data
// residency is configured via the object-store region/endpoint (spec §5.8).
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// env prefix shared by all FMS runtime variables.
const (
	envPrefix = "FMS_"

	// DefaultMinTLSVersion is the minimum accepted TLS protocol version.
	DefaultMinTLSVersion = "1.3"
	// DefaultArgon2Memory is the Argon2 memory parameter in KiB (64 MiB).
	DefaultArgon2Memory uint32 = 64 * 1024
	// DefaultArgon2Iterations is the Argon2 iteration parameter.
	DefaultArgon2Iterations uint32 = 3
	// DefaultArgon2Parallelism is the Argon2 parallelism parameter.
	DefaultArgon2Parallelism uint8 = 2
	// DefaultArgon2SaltLength is the Argon2 salt length in bytes.
	DefaultArgon2SaltLength uint32 = 16
	// DefaultArgon2KeyLength is the Argon2 derived-key length in bytes.
	DefaultArgon2KeyLength uint32 = 32
)

// Database holds the connection and pool tuning knobs for the primary ACID
// ledger store (PostgreSQL). Pool values map to §9.4.
type Database struct {
	URL          string
	PoolSize     int
	MaxOverflow  int
	PoolRecycle  time.Duration
	ConnTimeout  time.Duration
}

// Redis holds the cache / rate-limiting connection settings (§9.4).
type Redis struct {
	URL      string
	PoolSize int
}

// Amqp holds the durable message broker connection (§7.4).
type Amqp struct {
	URL string
}

// ObjectStore holds the S3-compatible object-store settings. Endpoint and
// Region are the data-residency knobs (§5.8); PresignTTL bounds the lifetime
// of pre-signed download links (§9.4).
type ObjectStore struct {
	Endpoint   string
	Region     string
	Bucket     string
	PresignTTL time.Duration
}

// Argon2Params are the adaptive-hashing parameters used by the key-derivation
// strategy for password storage (§9.1).
type Argon2Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// Security holds TLS material, authentication token lifecycles, session
// timeout, and password-hashing parameters (§5.1, §9.1).
type Security struct {
	TLSKey           string
	Cert             string
	JWTSecret        string
	Issuer           string
	MinTLSVersion    string
	AccessTokenTTL   time.Duration
	RefreshTokenTTL  time.Duration
	SessionTimeout   time.Duration
	Argon2Params     Argon2Params
}

// Audit holds the append-only audit retention policy and archival bucket.
// Exceeding retention triggers archival, never deletion (§5.3, §6.3).
type Audit struct {
	Retention     time.Duration
	ArchiveBucket string
}

// Accounting holds the base currency, single system time zone, and decimal
// precision applied to monetary values (§6.3, §7.3).
type Accounting struct {
	BaseCurrency   string
	SystemTimeZone string
	MaxDecimals    int
}

// HTTP holds the listener address and graceful-shutdown drain timeout.
type HTTP struct {
	Addr            string
	ShutdownTimeout time.Duration
}

// Observability holds the distributed-tracing knobs for the OpenTelemetry
// pipeline (§9.2). OtelEndpoint is the gRPC collector target (host:port, TLS
// in transit); when empty, tracing falls back to a no-op exporter so local and
// dev runs carry zero network egress. ServiceName must be "fms-api" or
// "fms-worker". TraceSampleRatio is the head-sampling ratio for the delegated
// TraceIDRatio sampler (0.0..1.0), overridable per service via feature flag.
type Observability struct {
	OtelEndpoint    string
	ServiceName     string
	TraceSampleRatio float64
}

// Config is the single runtime configuration surface. Consumers must receive
// only the relevant sub-struct, never the root, per the Interface Segregation
// Principle.
type Config struct {
	Database      Database
	Redis         Redis
	Amqp          Amqp
	ObjectStore   ObjectStore
	Security      Security
	Audit         Audit
	Accounting    Accounting
	HTTP          HTTP
	Observability Observability
	Flags         FeatureFlags
}

// Defaults returns a Config populated with the safe production defaults. No
// secret defaults are set here; JWTSecret, TLS material, and connection URLs
// must always come from the environment.
func Defaults() *Config {
	return &Config{
		Database: Database{
			PoolSize:    10,
			MaxOverflow: 20,
			PoolRecycle: 300 * time.Second,
			ConnTimeout: 5 * time.Second,
		},
		Redis: Redis{
			PoolSize: 10,
		},
		ObjectStore: ObjectStore{
			PresignTTL: 15 * time.Minute,
		},
		Security: Security{
			MinTLSVersion:  DefaultMinTLSVersion,
			AccessTokenTTL: 15 * time.Minute,
			SessionTimeout: 30 * time.Minute,
			Argon2Params: Argon2Params{
				Memory:      DefaultArgon2Memory,
				Iterations:  DefaultArgon2Iterations,
				Parallelism: DefaultArgon2Parallelism,
				SaltLength:  DefaultArgon2SaltLength,
				KeyLength:   DefaultArgon2KeyLength,
			},
		},
		Accounting: Accounting{
			BaseCurrency: "USD",
			MaxDecimals:  2,
		},
		HTTP: HTTP{
			Addr:            ":8080",
			ShutdownTimeout: 10 * time.Second,
		},
		Observability: Observability{
			ServiceName:     "fms-api",
			TraceSampleRatio: 1.0,
		},
		Flags: FeatureFlags{},
	}
}

// Load reads configuration from the process environment and validates it,
// failing fast on the first aggregate error. It is the entry point used by
// main and delegates to LoadFrom for testability.
func Load() (*Config, error) {
	return LoadFrom(os.LookupEnv)
}

// LoadFrom builds a Config from the supplied environment source, applies
// defaults, then validates the result (fail-fast). The getenv function lets
// tests inject a map instead of mutating the process environment.
func LoadFrom(getenv func(string) (string, bool)) (*Config, error) {
	cfg := Defaults()
	var errs []error

	cfg.Database.URL = envString(getenv, "FMS_DB_URL", cfg.Database.URL)
	envInt(getenv, "FMS_DB_POOL_SIZE", cfg.Database.PoolSize, &cfg.Database.PoolSize, &errs)
	envInt(getenv, "FMS_DB_MAX_OVERFLOW", cfg.Database.MaxOverflow, &cfg.Database.MaxOverflow, &errs)
	envDuration(getenv, "FMS_DB_POOL_RECYCLE", cfg.Database.PoolRecycle, &cfg.Database.PoolRecycle, &errs)
	envDuration(getenv, "FMS_DB_CONN_TIMEOUT", cfg.Database.ConnTimeout, &cfg.Database.ConnTimeout, &errs)

	cfg.Redis.URL = envString(getenv, "FMS_REDIS_URL", cfg.Redis.URL)
	envInt(getenv, "FMS_REDIS_POOL_SIZE", cfg.Redis.PoolSize, &cfg.Redis.PoolSize, &errs)

	cfg.Amqp.URL = envString(getenv, "FMS_AMQP_URL", cfg.Amqp.URL)

	cfg.ObjectStore.Endpoint = envString(getenv, "FMS_S3_ENDPOINT", cfg.ObjectStore.Endpoint)
	cfg.ObjectStore.Region = envString(getenv, "FMS_S3_REGION", cfg.ObjectStore.Region)
	cfg.ObjectStore.Bucket = envString(getenv, "FMS_S3_BUCKET", cfg.ObjectStore.Bucket)
	envDuration(getenv, "FMS_S3_PRESIGN_TTL", cfg.ObjectStore.PresignTTL, &cfg.ObjectStore.PresignTTL, &errs)

	cfg.Security.TLSKey = envString(getenv, "FMS_TLS_KEY", cfg.Security.TLSKey)
	cfg.Security.Cert = envString(getenv, "FMS_TLS_CERT", cfg.Security.Cert)
	cfg.Security.JWTSecret = envString(getenv, "FMS_JWT_SECRET", cfg.Security.JWTSecret)
	cfg.Security.Issuer = envString(getenv, "FMS_JWT_ISSUER", cfg.Security.Issuer)
	cfg.Security.MinTLSVersion = envString(getenv, "FMS_MIN_TLS_VERSION", cfg.Security.MinTLSVersion)
	envDuration(getenv, "FMS_ACCESS_TOKEN_TTL", cfg.Security.AccessTokenTTL, &cfg.Security.AccessTokenTTL, &errs)
	envDuration(getenv, "FMS_REFRESH_TOKEN_TTL", cfg.Security.RefreshTokenTTL, &cfg.Security.RefreshTokenTTL, &errs)
	envDuration(getenv, "FMS_SESSION_TIMEOUT", cfg.Security.SessionTimeout, &cfg.Security.SessionTimeout, &errs)

	envUint(getenv, "FMS_ARGON2_MEMORY", cfg.Security.Argon2Params.Memory, &cfg.Security.Argon2Params.Memory, &errs)
	envUint(getenv, "FMS_ARGON2_ITERATIONS", cfg.Security.Argon2Params.Iterations, &cfg.Security.Argon2Params.Iterations, &errs)
	envUint8(getenv, "FMS_ARGON2_PARALLELISM", cfg.Security.Argon2Params.Parallelism, &cfg.Security.Argon2Params.Parallelism, &errs)
	envUint(getenv, "FMS_ARGON2_SALT_LENGTH", cfg.Security.Argon2Params.SaltLength, &cfg.Security.Argon2Params.SaltLength, &errs)
	envUint(getenv, "FMS_ARGON2_KEY_LENGTH", cfg.Security.Argon2Params.KeyLength, &cfg.Security.Argon2Params.KeyLength, &errs)

	envDuration(getenv, "FMS_AUDIT_RETENTION", cfg.Audit.Retention, &cfg.Audit.Retention, &errs)
	cfg.Audit.ArchiveBucket = envString(getenv, "FMS_AUDIT_ARCHIVE_BUCKET", cfg.Audit.ArchiveBucket)

	cfg.Accounting.BaseCurrency = envString(getenv, "FMS_BASE_CURRENCY", cfg.Accounting.BaseCurrency)
	cfg.Accounting.SystemTimeZone = envString(getenv, "FMS_SYSTEM_TIMEZONE", cfg.Accounting.SystemTimeZone)
	envInt(getenv, "FMS_MAX_DECIMALS", cfg.Accounting.MaxDecimals, &cfg.Accounting.MaxDecimals, &errs)

	cfg.HTTP.Addr = envString(getenv, "FMS_HTTP_ADDR", cfg.HTTP.Addr)
	envDuration(getenv, "FMS_HTTP_SHUTDOWN_TIMEOUT", cfg.HTTP.ShutdownTimeout, &cfg.HTTP.ShutdownTimeout, &errs)

	cfg.Observability.OtelEndpoint = envString(getenv, "FMS_OTLP_ENDPOINT", cfg.Observability.OtelEndpoint)
	cfg.Observability.ServiceName = envString(getenv, "FMS_SERVICE_NAME", cfg.Observability.ServiceName)
	envFloat(getenv, "FMS_TRACE_SAMPLE_RATIO", cfg.Observability.TraceSampleRatio, &cfg.Observability.TraceSampleRatio, &errs)

	// Feature flags for contested scope: multi-currency (§9.5), audit read
	// logging (§9.4), notifications (§9.6), and backdated budgets. All default
	// to off; only explicit "true" enables them.
	envBool(getenv, "FMS_MULTI_CURRENCY", &cfg.Flags.multiCurrencyEnabled, &errs)
	envBool(getenv, "FMS_AUDIT_READ_LOGGING", &cfg.Flags.auditReadLoggingEnabled, &errs)
	envBool(getenv, "FMS_NOTIFICATIONS", &cfg.Flags.notificationsEnabled, &errs)
	envBool(getenv, "FMS_BACKDATED_BUDGETS", &cfg.Flags.backdatedBudgetsEnabled, &errs)

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// envString returns the value of name if present, otherwise def.
func envString(getenv func(string) (string, bool), name, def string) string {
	if v, ok := getenv(name); ok {
		return v
	}
	return def
}

// envInt parses an integer from the environment using ParseInt; it records a
// parse error on the errs slice rather than aborting the whole load.
func envInt(getenv func(string) (string, bool), name string, def int, dst *int, errs *[]error) {
	v, ok := getenv(name)
	if !ok {
		*dst = def
		return
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be an integer", name))
		return
	}
	*dst = int(n)
}

// envUint parses an unsigned 32-bit integer using ParseUint.
func envUint(getenv func(string) (string, bool), name string, def uint32, dst *uint32, errs *[]error) {
	v, ok := getenv(name)
	if !ok {
		*dst = def
		return
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be an unsigned integer", name))
		return
	}
	*dst = uint32(n)
}

// envUint8 parses an unsigned 8-bit integer using ParseUint.
func envUint8(getenv func(string) (string, bool), name string, def uint8, dst *uint8, errs *[]error) {
	v, ok := getenv(name)
	if !ok {
		*dst = def
		return
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 8)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be an unsigned integer", name))
		return
	}
	*dst = uint8(n)
}

// envDuration parses a duration via time.ParseDuration; on failure it records
// an error referencing only the variable name, never the offending value.
func envDuration(getenv func(string) (string, bool), name string, def time.Duration, dst *time.Duration, errs *[]error) {
	v, ok := getenv(name)
	if !ok {
		*dst = def
		return
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be a valid duration", name))
		return
	}
	*dst = d
}

// envFloat parses a float from the environment using ParseFloat; it records a
// parse error on the errs slice rather than aborting the whole load.
func envFloat(getenv func(string) (string, bool), name string, def float64, dst *float64, errs *[]error) {
	v, ok := getenv(name)
	if !ok {
		*dst = def
		return
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be a number", name))
		return
	}
	*dst = f
}

// envBool parses a strict boolean from the environment. Anything other than
// "true" or "false" (case-insensitive) is rejected rather than silently
// coerced, so a typo cannot silently flip a feature flag.
func envBool(getenv func(string) (string, bool), name string, dst *bool, errs *[]error) {
	v, ok := getenv(name)
	if !ok {
		*dst = false
		return
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true":
		*dst = true
	case "false":
		*dst = false
	default:
		*errs = append(*errs, fmt.Errorf("%s must be true or false", name))
	}
}

// Validate checks the loaded configuration and returns a single joined error
// describing every invalid field. It never echoes secret values or raw
// connection URLs with credentials. The offending variable name is reported,
// with generic guidance only.
func (c *Config) Validate() error {
	var errs []error
	appendIf := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	appendIf(required("FMS_DB_URL", c.Database.URL))
	appendIf(required("FMS_REDIS_URL", c.Redis.URL))
	appendIf(required("FMS_AMQP_URL", c.Amqp.URL))
	appendIf(required("FMS_S3_ENDPOINT", c.ObjectStore.Endpoint))
	appendIf(required("FMS_S3_REGION", c.ObjectStore.Region))
	appendIf(required("FMS_S3_BUCKET", c.ObjectStore.Bucket))
	appendIf(required("FMS_JWT_SECRET", c.Security.JWTSecret))
	appendIf(required("FMS_JWT_ISSUER", c.Security.Issuer))
	appendIf(required("FMS_SYSTEM_TIMEZONE", c.Accounting.SystemTimeZone))

	if len(c.Security.JWTSecret) < 32 {
		appendIf(fmt.Errorf("FMS_JWT_SECRET must be at least 32 bytes for 256-bit strength"))
	}

	if !hasDuration(c.Security.RefreshTokenTTL) {
		appendIf(fmt.Errorf("FMS_REFRESH_TOKEN_TTL is required and must be greater than zero"))
	}
	appendIf(positiveDuration("FMS_DB_POOL_RECYCLE", c.Database.PoolRecycle))
	appendIf(positiveDuration("FMS_DB_CONN_TIMEOUT", c.Database.ConnTimeout))
	appendIf(positiveDuration("FMS_S3_PRESIGN_TTL", c.ObjectStore.PresignTTL))
	appendIf(positiveDuration("FMS_ACCESS_TOKEN_TTL", c.Security.AccessTokenTTL))
	appendIf(positiveDuration("FMS_SESSION_TIMEOUT", c.Security.SessionTimeout))
	appendIf(positiveDuration("FMS_AUDIT_RETENTION", c.Audit.Retention))
	appendIf(positiveDuration("FMS_HTTP_SHUTDOWN_TIMEOUT", c.HTTP.ShutdownTimeout))

	if c.ObjectStore.PresignTTL > 30*time.Minute {
		appendIf(fmt.Errorf("FMS_S3_PRESIGN_TTL must not exceed 30m"))
	}
	if c.Security.AccessTokenTTL > 30*time.Minute {
		appendIf(fmt.Errorf("FMS_ACCESS_TOKEN_TTL must not exceed 30m"))
	}
	if c.Security.RefreshTokenTTL > 0 && c.Security.AccessTokenTTL > 0 && c.Security.RefreshTokenTTL <= c.Security.AccessTokenTTL {
		appendIf(fmt.Errorf("FMS_REFRESH_TOKEN_TTL must be greater than FMS_ACCESS_TOKEN_TTL"))
	}

	if c.Database.PoolSize <= 0 {
		appendIf(fmt.Errorf("FMS_DB_POOL_SIZE must be greater than zero"))
	}
	if c.Database.MaxOverflow < 0 {
		appendIf(fmt.Errorf("FMS_DB_MAX_OVERFLOW must not be negative"))
	}
	if c.Database.MaxOverflow < c.Database.PoolSize {
		appendIf(fmt.Errorf("FMS_DB_MAX_OVERFLOW must be >= FMS_DB_POOL_SIZE"))
	}
	if c.Redis.PoolSize <= 0 {
		appendIf(fmt.Errorf("FMS_REDIS_POOL_SIZE must be greater than zero"))
	}

	appendIf(validateMaxDecimals(c.Accounting.MaxDecimals))

	if _, err := time.LoadLocation(c.Accounting.SystemTimeZone); err != nil {
		appendIf(fmt.Errorf("FMS_SYSTEM_TIMEZONE must be a valid IANA time-zone name"))
	}

	if c.Observability.ServiceName != "fms-api" && c.Observability.ServiceName != "fms-worker" {
		appendIf(fmt.Errorf("FMS_SERVICE_NAME must be fms-api or fms-worker"))
	}
	if c.Observability.TraceSampleRatio < 0 || c.Observability.TraceSampleRatio > 1 {
		appendIf(fmt.Errorf("FMS_TRACE_SAMPLE_RATIO must be between 0 and 1"))
	}
	appendIf(validateOtelEndpoint(c.Observability.OtelEndpoint))

	appendIf(validateTLS(c.Security.TLSKey, c.Security.Cert))
	appendIf(validateMinTLS(c.Security.MinTLSVersion))
	appendIf(validateArgon2(c.Security.Argon2Params))

	// All feature flags are expected to be toggled by validation already; no
	// insecure default is introduced, so nothing further is required here.

	return errors.Join(errs...)
}

// required reports an error when v is empty, referencing the variable name
// only so error output never exposes a value (which may embed credentials).
func required(name, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

// hasDuration reports whether d is strictly positive.
func hasDuration(d time.Duration) bool { return d > 0 }

// positiveDuration validates that d is > 0.
func positiveDuration(name string, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("%s must be greater than zero", name)
	}
	return nil
}

// validateMaxDecimals bounds the monetary decimal precision to 1..4 so money
// is never represented with binary-float drift (§7.3).
func validateMaxDecimals(n int) error {
	if n < 1 || n > 4 {
		return fmt.Errorf("FMS_MAX_DECIMALS must be between 1 and 4")
	}
	return nil
}

// validateOtelEndpoint requires a non-empty OTLP endpoint to be a TLS-in-
// transit gRPC target per security posture. Scheme-less host:port is accepted
// (the exporter dials with explicit TLS credentials); bare plaintext schemes
// are rejected so traces are never exfiltrated in transit.
func validateOtelEndpoint(ep string) error {
	if ep == "" {
		return nil
	}
	lower := strings.ToLower(ep)
	if strings.Contains(lower, "://") {
		for _, allowed := range []string{"https://", "grpcs://", "dns://", "unix://"} {
			if strings.HasPrefix(lower, allowed) {
				return nil
			}
		}
		return fmt.Errorf("FMS_OTLP_ENDPOINT must use a TLS transport or dns/unix scheme")
	}
	if _, _, err := net.SplitHostPort(ep); err != nil {
		return fmt.Errorf("FMS_OTLP_ENDPOINT must be a host:port gRPC target")
	}
	return nil
}

// validateTLS requires the TLS key/cert pair to be both present or both
// absent, and, when present, readable with a private key that is not
// world-readable.
func validateTLS(key, cert string) error {
	keySet := key != ""
	certSet := cert != ""
	if keySet != certSet {
		return fmt.Errorf("FMS_TLS_KEY and FMS_TLS_CERT must both be set or both be empty")
	}
	if !keySet {
		return nil
	}
	for _, p := range []string{key, cert} {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			return fmt.Errorf("TLS material path is not a readable file: %s", filepath.Base(p))
		}
	}
	info, err := os.Stat(key)
	if err != nil {
		return fmt.Errorf("cannot inspect TLS key: %s", filepath.Base(key))
	}
	// Reject world-readable keys (0644/0666); a private key must be restricted
	// to the owning user (mode bits 0077 must be zero).
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("TLS key must not be world-readable: %s", filepath.Base(key))
	}
	return nil
}

// validateMinTLS rejects deprecated TLS versions (1.0/1.1) and any value
// outside the accepted allowlist to preserve PCI/regulatory posture.
func validateMinTLS(v string) error {
	switch v {
	case "1.2", "1.3":
		return nil
	default:
		return fmt.Errorf("FMS_MIN_TLS_VERSION must be 1.2 or 1.3")
	}
}

// validateArgon2 enforces the OWASP minimum floor for Argon2id parameters.
func validateArgon2(p Argon2Params) error {
	switch {
	case p.Memory < 19*1024:
		return fmt.Errorf("FMS_ARGON2_MEMORY must be at least 19 MiB (19456)")
	case p.Iterations < 2:
		return fmt.Errorf("FMS_ARGON2_ITERATIONS must be at least 2")
	case p.Parallelism < 1:
		return fmt.Errorf("FMS_ARGON2_PARALLELISM must be at least 1")
	case p.SaltLength < 8:
		return fmt.Errorf("FMS_ARGON2_SALT_LENGTH must be at least 8")
	case p.KeyLength < 16:
		return fmt.Errorf("FMS_ARGON2_KEY_LENGTH must be at least 16")
	default:
		return nil
	}
}

// redactURL strips any userinfo credentials from a connection URL so logs and
// error output never expose username/password material.
func redactURL(s string) string {
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return s
	}
	return s[:i+3] + "***@" + rest[at+1:]
}

// SafeString is the only representation of Config intended for logging. It
// exposes non-secret operational fields and redacts secrets, TLS key material,
// and the credential-bearing userinfo of every connection URL. Never log the
// Config value directly.
func (c Config) SafeString() string {
	return fmt.Sprintf(
		"Config{db_pool_size=%d db_max_overflow=%d db_pool_recycle=%s db_conn_timeout=%s "+
			"redis_pool_size=%d db_url=%s redis_url=%s amqp_url=%s "+
			"object_store{endpoint=%s region=%s bucket=%s presign_ttl=%s} "+
			"jwt_secret=[REDACTED] access_ttl=%s refresh_ttl=%s session_timeout=%s min_tls=%s "+
			"audit{retention=%s archive_bucket=%s} "+
			"accounting{base_currency=%s timezone=%s max_decimals=%d} "+
			"http{addr=%s shutdown_timeout=%s} "+
			"observability{service=%s otlp_endpoint=%s trace_ratio=%.2f}",
		c.Database.PoolSize, c.Database.MaxOverflow, c.Database.PoolRecycle, c.Database.ConnTimeout,
		c.Redis.PoolSize,
		redactURL(c.Database.URL), redactURL(c.Redis.URL), redactURL(c.Amqp.URL),
		c.ObjectStore.Endpoint, c.ObjectStore.Region, c.ObjectStore.Bucket, c.ObjectStore.PresignTTL,
		c.Security.AccessTokenTTL, c.Security.RefreshTokenTTL, c.Security.SessionTimeout, c.Security.MinTLSVersion,
		c.Audit.Retention, c.Audit.ArchiveBucket,
		c.Accounting.BaseCurrency, c.Accounting.SystemTimeZone, c.Accounting.MaxDecimals,
		c.HTTP.Addr, c.HTTP.ShutdownTimeout,
		c.Observability.ServiceName, redactURL(c.Observability.OtelEndpoint), c.Observability.TraceSampleRatio,
	)
}