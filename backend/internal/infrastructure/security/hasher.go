// Package security provides concrete implementations of the Identity & Access
// Management output ports. The Argon2id password hasher and the TOTP/JWT
// adapters live here so authentication use cases depend only on the ports in
// application/ports and never on a concrete cryptographic library.
package security

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/config"
)

// Hard bounds applied both at construction (new hashes) and when parsing a
// stored PHC string (existing hashes). The ceilings bound the CPU/memory cost a
// Verify call can be induced to spend when a stored hash is supplied by an
// untrusted path, preventing a resource-exhaustion amplification vector (CWE-770).
const (
	minMemoryKiB   = 19 * 1024  // OWASP floor for Argon2id memory (19 MiB).
	maxMemoryKiB   = 128 * 1024 // Ceiling beyond which a stored hash is rejected.
	minIterations  = 2          // OWASP floor; a single pass weakens memory-hardness.
	maxIterations  = 10         // Ceiling bounding per-Verify CPU time.
	minParallelism = 1
	maxParallelism = 4
	minSaltLen     = 16
	maxSaltLen     = 32
	minKeyLen      = 32 // 256-bit derived key.
	maxKeyLen      = 64
	argon2Version  = 0x13 // v=19 in the PHC string (Argon2 v1.3).

	// maxPasswordLen bounds the plaintext passed to Derive before Argon2 so a
	// single oversized credential request cannot dominate a hashing worker.
	maxPasswordLen = 128
	// maxEncodedLen matches the varchar(255) column contract for stored hashes
	// and bounds the size of a PHC string accepted by Verify.
	maxEncodedLen = 255
)

// argon2Params carries the decoded cost parameters of a stored PHC string.
type argon2Params struct {
	mem     uint32
	time    uint32
	threads uint8
	saltLen uint32
	keyLen  uint32
}

// Argon2Hasher generates and verifies Argon2id password hashes encoded as
// self-describing PHC strings using the adaptive-cost parameters supplied by
// config. It is immutable after construction and therefore safe for concurrent
// use; no mutable state is shared across calls.
type Argon2Hasher struct {
	mem     uint32
	time    uint32
	threads uint8
	saltLen uint32
	keyLen  uint32
}

var _ ports.PasswordHasher = (*Argon2Hasher)(nil)

// NewArgon2Hasher builds a hasher from the configured Argon2 parameters,
// failing fast (panicking) on values outside the validated hard bounds so a
// misconfiguration surfaces at startup rather than producing weak hashes at
// runtime. It logs a structured warning whenever a non-default parameter is in
// effect, for audit visibility.
func NewArgon2Hasher(cfg *config.Config) *Argon2Hasher {
	p := cfg.Security.Argon2Params
	if err := validateParams(p.Memory, p.Iterations, uint32(p.Parallelism), p.SaltLength, p.KeyLength); err != nil {
		panic(fmt.Sprintf("security: invalid Argon2 configuration: %v", err))
	}

	h := &Argon2Hasher{
		mem:     p.Memory,
		time:    p.Iterations,
		threads: p.Parallelism,
		saltLen: p.SaltLength,
		keyLen:  p.KeyLength,
	}
	if p.Memory != config.DefaultArgon2Memory ||
		p.Iterations != config.DefaultArgon2Iterations ||
		p.Parallelism != config.DefaultArgon2Parallelism ||
		p.SaltLength != config.DefaultArgon2SaltLength ||
		p.KeyLength != config.DefaultArgon2KeyLength {
		slog.Warn("Argon2 cost parameters differ from defaults",
			"memory_kib", p.Memory, "iterations", p.Iterations,
			"parallelism", p.Parallelism, "salt_length", p.SaltLength, "key_length", p.KeyLength)
	}
	return h
}

// Hash derives an Argon2id PHC string from password using a fresh random salt.
// The returned string is self-describing ($argon2id$v=19$m=..,t=..,p=..$salt$key)
// so cost parameters can be raised later without invalidating stored hashes.
// Passwords are never included in any returned value or error text.
func (h *Argon2Hasher) Hash(_ context.Context, password string) (string, error) {
	if len(password) == 0 || len(password) > maxPasswordLen {
		return "", errors.New("security: password length must be between 1 and 128 bytes")
	}

	salt := make([]byte, h.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("security: failed to generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.time, h.mem, h.threads, h.keyLen)

	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		h.mem, h.time, h.threads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify recomputes the Argon2id key with the cost parameters embedded in the
// stored hash and compares the derived bytes in constant time. It returns
// (false, nil) on a plain mismatch and (false, ports.ErrInvalidHash) only when
// the encoded value is malformed or structurally invalid. A malformed value
// short-circuits before the Argon2 derivation, so a parse failure has a shorter
// wall-clock cost than a genuine wrong-password attempt; this delta is accepted
// and bounded by the auth rate limit rather than closed with simulated work.
func (h *Argon2Hasher) Verify(_ context.Context, hash, password string) (bool, error) {
	if len(hash) == 0 || len(hash) > maxEncodedLen {
		return false, ports.ErrInvalidHash
	}
	if len(password) == 0 || len(password) > maxPasswordLen {
		return false, nil
	}

	p, salt, storedKey, err := parsePHC(hash)
	if err != nil {
		return false, ports.ErrInvalidHash
	}
	derivedKey := argon2.IDKey([]byte(password), salt, p.time, p.mem, p.threads, p.keyLen)

	// Only the derived bytes are compared; metadata is never compared so no
	// timing signal leaks about the stored parameters.
	if len(derivedKey) != len(storedKey) {
		return false, ports.ErrInvalidHash
	}
	match := subtle.ConstantTimeCompare(derivedKey, storedKey)
	return match == 1, false
}

// NeedsRehash reports whether the stored hash was derived with cost parameters
// below the hasher's current configured defaults, so the caller can transparently
// upgrade legacy hashes on the next successful login. A malformed hash returns
// (false, ports.ErrInvalidHash).
func (h *Argon2Hasher) NeedsRehash(_ context.Context, hash string) (bool, error) {
	p, _, _, err := parsePHC(hash)
	if err != nil {
		return false, ports.ErrInvalidHash
	}
	// Only a strictly weaker stored configuration triggers an upgrade; a hash
	// already as strong as or stronger than the current defaults is left alone.
	return p.mem < h.mem || p.time < h.time || p.threads < h.threads, nil
}

// validateParams enforces the hard floors and ceilings for new hashes at
// construction. Ceilings mirror the bounds applied when parsing stored hashes.
func validateParams(mem, iter, threads, saltLen, keyLen uint32) error {
	switch {
	case mem < minMemoryKiB:
		return fmt.Errorf("memory must be at least %d KiB, got %d", minMemoryKiB, mem)
	case mem > maxMemoryKiB:
		return fmt.Errorf("memory must be at most %d KiB, got %d", maxMemoryKiB, mem)
	case iter < minIterations:
		return fmt.Errorf("iterations must be at least %d, got %d", minIterations, iter)
	case iter > maxIterations:
		return fmt.Errorf("iterations must be at most %d, got %d", maxIterations, iter)
	case threads < minParallelism || threads > maxParallelism:
		return fmt.Errorf("parallelism must be between %d and %d, got %d", minParallelism, maxParallelism, threads)
	case saltLen < minSaltLen:
		return fmt.Errorf("salt length must be at least %d bytes, got %d", minSaltLen, saltLen)
	case saltLen > maxSaltLen:
		return fmt.Errorf("salt length must be at most %d bytes, got %d", maxSaltLen, saltLen)
	case keyLen < minKeyLen:
		return fmt.Errorf("key length must be at least %d bytes, got %d", minKeyLen, keyLen)
	case keyLen > maxKeyLen:
		return fmt.Errorf("key length must be at most %d bytes, got %d", maxKeyLen, keyLen)
	default:
		return nil
	}
}

// parsePHC decodes a PHC string produced by Hash. It accepts only the exact
// v=19 Argon2id shape, parses cost fields without sign or whitespace tolerance,
// rejects any parameter outside the hard bounds, and returns ErrInvalidHash for
// every structural anomaly. The key and salt are decoded from raw standard base64.
func parsePHC(encoded string) (*argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[2] != "v=19" {
		return nil, nil, nil, ports.ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return nil, nil, nil, ports.ErrInvalidHash
	}

	m, t, p, err := parseCostFields(parts[3])
	if err != nil {
		return nil, nil, nil, ports.ErrInvalidHash
	}

	// Decode and length-check salt and key only after confirming the cost params
	// are within bounds, so no oversized allocation is triggered by a hostile string.
	if m == 0 || m > maxMemoryKiB || t == 0 || t > maxIterations || p == 0 || p > maxParallelism {
		return nil, nil, nil, ports.ErrInvalidHash
	}
	if m < minMemoryKiB || t < minIterations {
		return nil, nil, nil, ports.ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return nil, nil, nil, ports.ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return nil, nil, nil, ports.ErrInvalidHash
	}
	if len(salt) == 0 || len(salt) > maxSaltLen || len(key) == 0 || len(key) > maxKeyLen {
		return nil, nil, nil, ports.ErrInvalidHash
	}

	params := &argon2Params{mem: m, time: t, threads: uint8(p), saltLen: uint32(len(salt)), keyLen: uint32(len(key))}
	return params, salt, key, nil
}

// parseCostFields parses the "m=...,t=...,p=..." parameter segment strictly,
// rejecting values that carry a sign or fail to parse as unsigned integers.
func parseCostFields(segment string) (mem, iter, threads uint32, err error) {
	fields := strings.Split(segment, ",")
	if len(fields) != 3 {
		return 0, 0, 0, ports.ErrInvalidHash
	}
	values := make(map[string]uint32, 3)
	for _, f := range fields {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			return 0, 0, 0, ports.ErrInvalidHash
		}
		v, perr := strconv.ParseUint(kv[1], 10, 32)
		if perr != nil {
			return 0, 0, 0, ports.ErrInvalidHash
		}
		values[kv[0]] = uint32(v)
	}
	m, ok := values["m"]
	if !ok {
		return 0, 0, 0, ports.ErrInvalidHash
	}
	t, ok := values["t"]
	if !ok {
		return 0, 0, 0, ports.ErrInvalidHash
	}
	p, ok := values["p"]
	if !ok {
		return 0, 0, 0, ports.ErrInvalidHash
	}
	return m, t, p, nil
}