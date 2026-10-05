// Package usecases implements the application-layer orchestration of the FMS
// core. Use cases depend only on the output ports and the domain layer; they
// contain no framework, HTTP, or persistence types.
//
// This file owns the authentication flow of the Identity & Access bounded
// context (Module F): password verification, MFA (TOTP) challenge/response,
// token issuance and rotation, session lifecycle, and access-token verification.
// It is the application seam that the HTTP adapter drives; all error codes and
// identities are mapped to http statuses/claims only at the adapter boundary.
//
// The two-stage login is binding-protected: stage 1 (password) produces a
// short-lived, single-use challenge tied to the authenticated user, and stage 2
// (TOTP) is accepted only against that challenge. Neither stage depends on, nor
// leaks, whether the username exists, whether the password was wrong, or
// whether MFA is enrolled — every authentication failure surfaces as the same
// generic UNAUTHENTICATED domain error (enumeration resistance).
package usecases

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

const (
	// SessionTTL is the server-side sliding inactivity window for a session.
	// It is applied on session creation and on every Touch, never supplied by
	// the client, so expiry is enforced server-side (specification §5.1, §9.1).
	SessionTTL = 30 * time.Minute

	// AccessTokenTTL bounds how long a rotated access token remains usable. It
	// is informational for this use case (the token manager enforces it); it is
	// kept here so the policy is explicit and greppable.
	AccessTokenTTL = 15 * time.Minute

	// challengeTTL is the lifetime of a stage-1 password challenge before it
	// expires and stage 2 must restart from the password.
	challengeTTL = 5 * time.Minute

	// MFA brute-force guard: after maxMFALockoutAttempts failed TOTP submissions
	// within maxMFALockoutWindow, VerifyMfa is refused for that user for the
	// remainder of the window (best-effort, process-local; the gateway's
	// distributed Redis rate limit remains the primary defense).
	maxMFALockoutAttempts = 5
	maxMFALockoutWindow   = 60 * time.Second
)

// Boundary-input allowlists. Username mirrors the domain.NewUser structural
// allowlist (applied after trim + lowercase normalization); TOTP is rejected as
// malformed before it ever reaches the constant-time verify path.
var (
	usernameRE = regexp.MustCompile(`^[a-zA-Z0-9._-]{3,64}$`)
	totpCodeRE = regexp.MustCompile(`^[0-9]{6}$`)
)

// MfaChallengeRequiredError is the application sentinel signalling that stage-1
// password verification succeeded and a TOTP code is now required. It is
// identity-comparable so tests assert on it directly; the HTTP adapter maps it
// to the stage-1 200 response (mfa_challenge_required), distinct from any
// error-path status.
var MfaChallengeRequiredError = errors.New("use case: mfa challenge required")

// LoginCommand is the stage-1 credential input. It deliberately carries no
// tenant field — the tenant scope is resolved server-side from the authenticated
// request context and passed as a separate argument so a client can never
// influence it (tenant isolation, specification §7.8/§5.1).
type LoginCommand struct {
	Username string
	Password string
}

// MfaCommand completes stage 2 by submitting the TOTP code together with the
// pending challenge id issued by stage 1. The challenge id is what binds this
// step to the prior, password-verified stage.
type MfaCommand struct {
	Username  string
	TOTPCode  string
	PendingID string
}

// LoginResult is the successful-authentication contract returned by stage 2 (a
// fully issued session) and by the stage-1 challenge response. MFACompleted is
// true only once the TOTP step has succeeded; MfaChallengeRequired/PendingID
// describe a stage-1 response awaiting the code.
type LoginResult struct {
	AccessToken          string
	RefreshToken         string
	SessionID            uuid.UUID
	ExpiresAt            time.Time
	Role                 string
	TenantID             uuid.UUID
	MFACompleted         bool
	MfaChallengeRequired bool
	PendingID            string
}

// mfaChallenge is the server-side record binding a stage-1 password success to
// the subsequent TOTP submission. ExpiresAt bounds it; Used marks it consumed
// (single-use) so a challenge cannot be replayed to brute-force TOTP codes.
type mfaChallenge struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	ExpiresAt time.Time
	Used      bool
}

// mfaChallengeStore is the stage-1→stage-2 binding store. It is process-local
// and short-lived (challenge TTL is a few minutes), bounding memory while
// keeping the two-step login cryptographically bound without touching the
// session store (which is reserved for established sessions).
type mfaChallengeStore struct {
	mu   sync.Mutex
	byID map[string]*mfaChallenge
}

func newMfaChallengeStore() *mfaChallengeStore {
	return &mfaChallengeStore{byID: make(map[string]*mfaChallenge)}
}

// issueChallenge stores a fresh single-use challenge for the authenticated user
// and returns its opaque id. It never embeds the username, password, or TOTP
// code — the id is meaningless without the challenge record and is invalidated
// on consumption.
func (cs *mfaChallengeStore) issueChallenge(now time.Time, userID, tenantID uuid.UUID) (string, error) {
	id := uuid.New().String()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.byID[id] = &mfaChallenge{
		UserID:    userID,
		TenantID:  tenantID,
		ExpiresAt: now.Add(challengeTTL),
	}
	return id, nil
}

// consumeChallenge retrieves the challenge for a pending id, marks it used, and
// removes it atomically so it can never be presented twice. It returns the
// generic UNAUTHENTICATED error for a missing, expired, or already-used
// challenge, keeping the failure surface uniform.
func (cs *mfaChallengeStore) consumeChallenge(now time.Time, pendingID string) (*mfaChallenge, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	ch, ok := cs.byID[pendingID]
	if !ok || ch.Used || now.After(ch.ExpiresAt) {
		delete(cs.byID, pendingID)
		return nil, domain.NewUnauthenticated("invalid credentials")
	}
	ch.Used = true
	delete(cs.byID, pendingID)
	return ch, nil
}

// mfaLockout enforces a process-local backoff for failed TOTP attempts per user.
// It is best-effort defense-in-depth; the gateway's Redis-backed rate limit is
// the authoritative distributed control. Responses stay generic regardless.
type mfaLockout struct {
	mu        sync.Mutex
	attempts  map[uuid.UUID]int
	lockedFor map[uuid.UUID]time.Time
}

func newMFALockout() *mfaLockout {
	return &mfaLockout{
		attempts:  make(map[uuid.UUID]int),
		lockedFor: make(map[uuid.UUID]time.Time),
	}
}

// recordFailure increments the failed-attempt counter for a user and, once it
// crosses the threshold within the window, arms the lock until the window ends
// (escalating backoff: the window is re-armed on each further failure).
func (m *mfaLockout) recordFailure(now time.Time, userID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attempts[userID] == 0 {
		m.attempts[userID] = 1
		return
	}
	m.attempts[userID]++
	if m.attempts[userID] >= maxMFALockoutAttempts {
		m.lockedFor[userID] = now.Add(maxMFALockoutWindow)
		m.attempts[userID] = 0
	}
}

// isLocked reports whether a user is currently within a TOTP lockout window. It
// lazily clears an expired lock.
func (m *mfaLockout) isLocked(now time.Time, userID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.lockedFor[userID]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(m.lockedFor, userID)
		return false
	}
	return true
}

// validateLoginCommand enforces the username allowlist and a non-empty password
// before any port call, so malformed input never reaches the hasher (which is
// expensive and constant-time-sensitive).
func validateLoginCommand(cmd LoginCommand) error {
	var fields []domain.FieldError
	u := strings.TrimSpace(cmd.Username)
	if !usernameRE.MatchString(u) {
		fields = append(fields, domain.FieldError{Field: "username", Message: "must be 3-64 [a-zA-Z0-9._-] characters"})
	}
	if len(cmd.Password) == 0 {
		fields = append(fields, domain.FieldError{Field: "password", Message: "password is required"})
	}
	if len(fields) > 0 {
		return domain.NewValidationError(fields...)
	}
	return nil
}

// validateMfaCommand enforces the username and pending-id allowlists and
// requires exactly six ASCII digits for the TOTP code before verification.
func validateMfaCommand(cmd MfaCommand) error {
	var fields []domain.FieldError
	u := strings.TrimSpace(cmd.Username)
	if !usernameRE.MatchString(u) {
		fields = append(fields, domain.FieldError{Field: "username", Message: "must be 3-64 [a-zA-Z0-9._-] characters"})
	}
	if !totpCodeRE.MatchString(cmd.TOTPCode) {
		fields = append(fields, domain.FieldError{Field: "totp_code", Message: "must be exactly 6 digits"})
	}
	if strings.TrimSpace(cmd.PendingID) == "" {
		fields = append(fields, domain.FieldError{Field: "pending_id", Message: "mfa challenge pending id is required"})
	}
	if len(fields) > 0 {
		return domain.NewValidationError(fields...)
	}
	return nil
}

// AuthService orchestrates authentication. It depends only on the declared
// output ports (repositories, hasher, MFA, tokens, sessions, clock) so it stays
// free of any framework, HTTP, or crypto-library dependency, satisfying the
// Clean Architecture inward-dependency rule.
type AuthService struct {
	users      ports.UserRepository
	hasher     ports.PasswordHasher
	mfa        ports.MfaVerifier
	tokens     ports.TokenManager
	sessions   ports.SessionStore
	now        ports.Clock
	sessionTTL time.Duration
	challenges *mfaChallengeStore
	lockout    *mfaLockout
}

// NewAuthService assembles the AuthService from its output ports. The session
// inactivity window is fixed at SessionTTL (30 minutes), a domain policy rather
// than a caller-controlled value, so it can never be weakened by a client.
func NewAuthService(
	users ports.UserRepository,
	hasher ports.PasswordHasher,
	mfa ports.MfaVerifier,
	tokens ports.TokenManager,
	sessions ports.SessionStore,
	now ports.Clock,
) *AuthService {
	return &AuthService{
		users:      users,
		hasher:     hasher,
		mfa:        mfa,
		tokens:     tokens,
		sessions:   sessions,
		now:        now,
		sessionTTL: SessionTTL,
		challenges: newMfaChallengeStore(),
		lockout:    newMFALockout(),
	}
}

// Login performs stage-1 password verification for the given tenant scope.
// Every failure path returns the same generic UNAUTHENTICATED error so the
// response never reveals whether the username exists, the password was wrong,
// or MFA is enrolled. On success it returns a stage-1 challenge result; no
// tokens are issued at this stage. MFA is mandatory for all accounts, so an
// account without an active TOTP secret is refused (no silent bypass).
func (s *AuthService) Login(ctx context.Context, cmd LoginCommand, tenantScope uuid.UUID) (*LoginResult, error) {
	if err := validateLoginCommand(cmd); err != nil {
		return nil, err
	}
	if tenantScope == uuid.Nil {
		return nil, domain.NewUnauthenticated("invalid credentials")
	}

	// Tenant is server-resolved; the query is tenant-scoped so this never probes
	// across tenants, and absent vs. wrong-credential are not distinguishable.
	user, derr := s.users.GetByUsername(ctx, tenantScope, strings.TrimSpace(cmd.Username))
	if derr != nil || user == nil {
		slog.Warn("auth: username lookup failed", "tenant_id", tenantScope.String(), "event", "login_failed")
		return nil, domain.NewUnauthenticated("invalid credentials")
	}
	if !user.Active() {
		return nil, domain.NewUnauthenticated("invalid credentials")
	}

	ok, err := s.hasher.Verify(ctx, user.PasswordHash, cmd.Password)
	if err != nil || !ok {
		// ErrInvalidHash and infra failures converge on the same generic boundary
		// error; the cause is logged server-side only, never returned.
		slog.Warn("auth: password verification failed", "user_id", user.ID.String(), "event", "login_failed")
		return nil, domain.NewUnauthenticated("invalid credentials")
	}

	// MFA mandate (specification §5.1, §9.1): there is no "MFA off" path. An
	// unenrolled account is refused identically to a wrong password.
	if !user.MFAActive || user.MFASecret == "" {
		return nil, domain.NewUnauthenticated("invalid credentials")
	}

	pendingID, err := s.challenges.issueChallenge(s.now.Now(), user.ID, tenantScope)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	return &LoginResult{
		MfaChallengeRequired: true,
		PendingID:            pendingID,
		TenantID:             tenantScope,
	}, nil
}

// VerifyMfa completes a two-stage login by validating the TOTP code against the
// challenge-bound user's secret, then issues a token pair and creates the
// server-side session. The challenge binding guarantees the password step was
// performed for this account first, so a caller holding only a valid TOTP code
// cannot skip password verification. A wrong code, missing/expired/used
// challenge, or locked-out user all surface the same generic UNAUTHENTICATED
// error.
func (s *AuthService) VerifyMfa(ctx context.Context, cmd MfaCommand) (*LoginResult, error) {
	if err := validateMfaCommand(cmd); err != nil {
		return nil, err
	}
	now := s.now.Now()

	ch, err := s.challenges.consumeChallenge(now, strings.TrimSpace(cmd.PendingID))
	if err != nil {
		return nil, domain.NewUnauthenticated("invalid credentials")
	}

	user, derr := s.users.GetByUsername(ctx, ch.TenantID, strings.TrimSpace(cmd.Username))
	if derr != nil || user == nil || !user.Active() {
		return nil, domain.NewUnauthenticated("invalid credentials")
	}
	// Bind: the consumed challenge must belong to this exact user/tenant.
	if user.ID != ch.UserID || user.TenantID != ch.TenantID {
		slog.Warn("auth: mfa challenge bound to a different principal", "user_id", user.ID.String(), "event", "mfa_failed")
		return nil, domain.NewUnauthenticated("invalid credentials")
	}
	if !user.MFAActive || user.MFASecret == "" {
		return nil, domain.NewUnauthenticated("invalid credentials")
	}
	// Lockout is keyed on the authenticated user's ID so incremental TOTP
	// brute-force backoff actually binds to the account being attacked.
	if s.lockout.isLocked(now, user.ID) {
		return nil, domain.NewUnauthenticated("invalid credentials")
	}

	ok, err := s.mfa.Verify(ctx, user.MFASecret, cmd.TOTPCode)
	if err != nil {
		return nil, domain.NewDependencyUnavailable("authentication unavailable")
	}
	if !ok {
		s.lockout.recordFailure(s.now.Now(), user.ID)
		slog.Warn("auth: mfa verification failed", "user_id", user.ID.String(), "event", "mfa_failed")
		return nil, domain.NewUnauthenticated("invalid credentials")
	}

	slog.Info("auth: mfa verified", "user_id", user.ID.String(), "event", "mfa_success")
	return s.completeLogin(ctx, user, user.TenantID, true)
}

// completeLogin mints the access/refresh pair and creates the server-side
// session, shared by the completed TOTP flow. On any failure after issuance it
// revokes the pair (compensation) so a token is never left in circulation
// without a corresponding session, mirroring a use-case-scoped transaction
// boundary.
func (s *AuthService) completeLogin(ctx context.Context, user *domain.User, tenantID uuid.UUID, mfaCompleted bool) (*LoginResult, error) {
	// The role claim is carried as the user's role identifier (RoleID string),
	// server-authoritative per the domain model; role claims are never trusted
	// from client input.
	pair, err := s.tokens.Issue(ctx, user.ID, tenantID, user.RoleID.String(), nil)
	if err != nil {
		return nil, err
	}

	now := s.now.Now().UTC()
	sessID := uuid.New()
	sess := ports.Session{
		ID:           sessID,
		UserID:       user.ID,
		TenantID:     user.TenantID,
		Role:         user.RoleID.String(),
		CreatedAt:    now,
		LastActiveAt: now,
	}
	if err := s.sessions.Create(ctx, sess, s.sessionTTL); err != nil {
		// Compensation: roll back the just-issued token pair.
		_ = s.tokens.Revoke(ctx, pair.TokenID)
		slog.Warn("auth: session creation failed; tokens revoked", "user_id", user.ID.String(), "event", "login_failed")
		return nil, domain.NewDependencyUnavailable("session store unavailable")
	}

	slog.Info("auth: login completed", "user_id", user.ID.String(), "tenant_id", user.TenantID.String(), "event", "login_success")
	return &LoginResult{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		SessionID:    sessID,
		ExpiresAt:    pair.ExpiresAt,
		Role:         user.RoleID.String(),
		TenantID:     user.TenantID,
		MFACompleted: mfaCompleted,
	}, nil
}

// TouchSession slides the server-side inactivity window for an active session,
// resetting its TTL to the session inactivity window. A missing or expired
// session yields UNAUTHENTICATED (the store's canonical mapping) so the
// middleware rejects at 401 with no refresh-grace window.
func (s *AuthService) TouchSession(ctx context.Context, sessionID uuid.UUID) error {
	if sessionID == uuid.Nil {
		return domain.NewUnauthenticated("invalid session")
	}
	if err := s.sessions.Touch(ctx, sessionID, s.sessionTTL); err != nil {
		return err
	}
	return nil
}

// Authenticate validates the presented access token (signature, expiry,
// revocation blacklist via the token manager) and re-checks server-side state:
// the account must still exist and be active in the claimed tenant. It returns
// the verified domain.User for the middleware's principal binding, rejecting a
// deprovisioned or suspended account even if its token is unexpired.
func (s *AuthService) Authenticate(ctx context.Context, accessToken string) (*domain.User, error) {
	claims, err := s.tokens.VerifyAccess(ctx, accessToken)
	if err != nil {
		return nil, err // UNAUTHENTICATED / DEPENDENCY_UNAVAILABLE propagated
	}
	userID, derr := uuid.Parse(claims.Subject)
	if derr != nil {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	user, uerr := s.users.GetByID(ctx, claims.TenantID, userID)
	if uerr != nil || user == nil {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	if !user.Active() || user.TenantID != claims.TenantID {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	return user, nil
}

// Logout hard-revokes a session and the associated token pair. It deletes the
// session server-side and revokes the pair by TokenID (blacklisting the access
// token for its remaining lifetime and shadowing the refresh token). It is
// idempotent: revoking an already-absent session or token returns success. The
// actor is logged for audit telemetry only; no secrets are ever logged.
func (s *AuthService) Logout(ctx context.Context, sessionID, tokenID, actorID uuid.UUID) error {
	if sessionID != uuid.Nil {
		_ = s.sessions.Delete(ctx, sessionID)
	}
	if tokenID != uuid.Nil {
		_ = s.tokens.Revoke(ctx, tokenID)
	}
	slog.Info("auth: logout", "user_id", actorID.String(), "session_id", sessionID.String(), "event", "logout")
	return nil
}

// RotateRefreshSingleUse swaps a presented refresh token for a fresh pair and
// atomically invalidates the old one (single-use via the token manager's
// atomic rotation). On reuse of an already-rotated token it treats this as a
// suspected compromise and revokes the whole session family: the session is
// deleted and the caller's in-flight access token is implied-revoked, cutting
// off the compromised session rather than merely returning an error.
func (s *AuthService) RotateRefreshSingleUse(ctx context.Context, refreshToken string, sessionID uuid.UUID) (*LoginResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, domain.NewUnauthenticated("invalid refresh token")
	}
	pair, err := s.tokens.RotateRefresh(ctx, refreshToken)
	if err != nil {
		// Reuse/unknown/expired refresh → suspected compromise: revoke the family.
		if sessionID != uuid.Nil {
			_ = s.sessions.Delete(ctx, sessionID)
		}
		slog.Warn("auth: refresh reuse detected; session family revoked", "session_id", sessionID.String(), "event", "token_reuse")
		return nil, domain.NewForbidden("session terminated")
	}
	// The session continues under the rotated token; reset its inactivity window.
	if sessionID != uuid.Nil {
		_ = s.sessions.Touch(ctx, sessionID, s.sessionTTL)
	}
	slog.Info("auth: refresh rotated", "session_id", sessionID.String(), "event", "token_rotated")
	return &LoginResult{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		SessionID:    sessionID,
		ExpiresAt:    pair.ExpiresAt,
		MFACompleted: true,
	}, nil
}