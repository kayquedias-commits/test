// Package security provides infrastructure adapters for the Identity & Access
// ports declared in internal/application/ports: password hashing, token
// management, and RFC 6238 TOTP multi-factor authentication.
package security

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/config"
)

const (
	// defaultMFAIssuer is the OTP issuer label used in the enrollment
	// provisioning URI when FMS_MFA_ISSUER is not set.
	defaultMFAIssuer = "FMS"
	// totpEnvIssuer selects the authenticator issuer label at runtime.
	totpEnvIssuer = "FMS_MFA_ISSUER"
	// totpPeriod is the RFC 6238 time step in seconds.
	totpPeriod = 30
)

// ErrInvalidCodeSyntax is the sentinel returned by ValidateCodeSyntax when the
// supplied code is not exactly six ASCII digits. Handlers map it to the
// VALIDATION_FAILED envelope with a field error for mfa_code.
var ErrInvalidCodeSyntax = errors.New("mfa code must be exactly 6 digits")

// codeRe is a strict allowlist: exactly six ASCII digits, no whitespace.
var codeRe = regexp.MustCompile(`^[0-9]{6}$`)

// TOTPMfa is the RFC 6238 TOTP adapter implementing ports.MfaVerifier. It is
// stateless: secrets are owned by the User aggregate and decrypted only inside
// the verification path; the adapter neither persists nor logs secrets.
type TOTPMfa struct {
	issuer string
}

// NewTOTPMfa returns a TOTP adapter. cfg is accepted for composition-root
// parity with the other security adapters; the issuer defaults to "FMS" and is
// overridable via FMS_MFA_ISSUER because config.Config carries no MFA field.
func NewTOTPMfa(cfg *config.Config) *TOTPMfa {
	_ = cfg
	issuer := os.Getenv(totpEnvIssuer)
	if issuer == "" {
		issuer = defaultMFAIssuer
	}
	return &TOTPMfa{issuer: issuer}
}

// Verify implements ports.MfaVerifier. It checks a six-digit TOTP code against
// the secret using SHA1, 6 digits, and a 30 s period with ±1 step skew (three
// candidate windows). Constant-time comparison is delegated to the library's
// HMAC path. It returns (false, nil) for any authentication failure (wrong,
// expired, or malformed code; corrupt secret) so callers cannot craft an
// oracle, and (false, err) only on infrastructure failure.
func (m *TOTPMfa) Verify(_ context.Context, secret, code string) (bool, error) {
	if secret == "" || !codeRe.MatchString(code) {
		return false, nil
	}
	ok, err := totp.ValidateCustom(code, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    totpPeriod,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		// A malformed stored secret surfaces as a uniform authentication
		// failure rather than leaking a decode error to the caller.
		return false, nil
	}
	return ok, nil
}

// GenerateSecret implements ports.MfaVerifier. It returns a base32-encoded
// 20-byte (160-bit) secret generated from crypto/rand, satisfying the RFC 4226
// entropy floor. Only enrollment consumes it; it is never logged or stored by
// this adapter.
func (m *TOTPMfa) GenerateSecret() (string, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      m.issuer,
		AccountName: "fms",
		Period:      totpPeriod,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", fmt.Errorf("generate totp secret: %w", err)
	}
	return key.Secret(), nil
}

// ValidateCodeSyntax enforces the strict allowlist before any cryptographic
// verification: exactly six ASCII digits, no trimming. Non-digit, wrong-length,
// or whitespace input returns ErrInvalidCodeSyntax so malformed codes never
// reach the decrypt/HMAC path.
func (m *TOTPMfa) ValidateCodeSyntax(code string) error {
	if !codeRe.MatchString(code) {
		return ErrInvalidCodeSyntax
	}
	return nil
}

// ProvisioningURI renders the otpauth enrollment URI (RFC 8905 key-URI format)
// for QR enrollment. The account and issuer labels are URL-escaped to prevent
// URI/QR-payload injection. The URI carries the plaintext secret and must be
// surfaced only once over TLS during enrollment.
func (m *TOTPMfa) ProvisioningURI(accountName, issuer, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(accountName)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return (&url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     label,
		RawQuery: q.Encode(),
	}).String()
}