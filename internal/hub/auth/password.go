package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. They are deliberately fixed for the whole hub so every
// password (and every unknown-username probe) costs exactly the same work.
// 32 MiB with three passes is the OWASP argon2id configuration scaled for a
// small self-hosted box; four concurrent logins therefore peak at about
// 128 MiB of transient hashing memory.
const (
	argonTime    = 3
	argonMemory  = 32 * 1024 // KiB
	argonThreads = 2
	argonSaltLen = 16
	argonKeyLen  = 32

	// Verification bounds. A stored hash that asks for more work than this is
	// refused instead of honoured, so a corrupted or hostile row cannot make
	// the hub allocate gigabytes or spin for minutes per login attempt.
	maxVerifyTime    = 10
	maxVerifyMemory  = 64 * 1024 // KiB (64 MiB per verification)
	maxVerifyThreads = 16
	maxVerifySalt    = 64
	maxVerifyKey     = 64
	maxVerifyWork    = 128 * 1024 // time*memory budget, in KiB-passes
	minVerifySalt    = 8
	minVerifyKey     = 16
)

// PasswordHashPrefix identifies the only accepted hash format. The stored
// value is a PHC string:
//
//	$argon2id$v=19$m=32768,t=3,p=2$<salt>$<key>
//
// with unpadded standard base64 for the salt and key.
const PasswordHashPrefix = "$argon2id$v=19$m=32768,t=3,p=2$"

// MaxPasswordChars is the browser password bound, counted in UTF-16 code
// units to match the reference hub's String.prototype.length checks.
const MaxPasswordChars = 1024

// ErrInvalidHash reports a stored password hash that is not a bounded
// argon2id value produced by this hub.
var ErrInvalidHash = errors.New("auth: stored password hash is not a supported argon2id value")

// HashPassword derives a new salted argon2id hash. The plaintext is never
// stored, logged or echoed. Input is hashed as the exact UTF-8 bytes the
// browser sent; no prototype hash is imported, so Unicode normalisation is not
// needed for compatibility here.
func HashPassword(password string) (string, error) {
	if n := utf16Len(password); n == 0 || n > MaxPasswordChars {
		return "", fmt.Errorf("password must be 1..%d characters", MaxPasswordChars)
	}
	var salt [argonSaltLen]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt[:], argonTime, argonMemory, argonThreads, argonKeyLen)
	encoded := base64.RawStdEncoding
	return PasswordHashPrefix +
		encoded.EncodeToString(salt[:]) + "$" +
		encoded.EncodeToString(key), nil
}

// ValidatePasswordHash reports whether encoded is a supported argon2id hash
// without doing password work. Startup uses it so a plaintext or foreign hash
// is refused before the hub accepts logins.
func ValidatePasswordHash(encoded string) error {
	_, _, _, err := parsePasswordHash(encoded)
	return err
}

// VerifyPassword compares a candidate password with a stored hash in constant
// time. Unknown-username probes pass a fixed dummy hash so they cost the same
// work as a real account.
func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, want, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	if n := utf16Len(password); n == 0 || n > MaxPasswordChars {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, nil
	}
	return true, nil
}

type argonParams struct {
	time    uint32
	memory  uint32
	threads uint8
}

func parsePasswordHash(encoded string) (argonParams, []byte, []byte, error) {
	if len(encoded) == 0 || len(encoded) > 4096 {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	// $argon2id$v=19$m=32768,t=3,p=2$salt$key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if parts[2] != "v=19" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	var params argonParams
	for _, field := range strings.Split(parts[3], ",") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return argonParams{}, nil, nil, ErrInvalidHash
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return argonParams{}, nil, nil, ErrInvalidHash
		}
		switch key {
		case "m":
			params.memory = uint32(parsed)
		case "t":
			params.time = uint32(parsed)
		case "p":
			params.threads = uint8(parsed)
		default:
			return argonParams{}, nil, nil, ErrInvalidHash
		}
	}
	if params.time < 1 || params.time > maxVerifyTime ||
		params.memory < 8*uint32(params.threads) || params.memory > maxVerifyMemory ||
		params.threads < 1 || params.threads > maxVerifyThreads {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if uint64(params.time)*uint64(params.memory) > maxVerifyWork {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	encoded64 := base64.RawStdEncoding
	salt, err := encoded64.DecodeString(parts[4])
	if err != nil || len(salt) < minVerifySalt || len(salt) > maxVerifySalt {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	key, err := encoded64.DecodeString(parts[5])
	if err != nil || len(key) < minVerifyKey || len(key) > maxVerifyKey {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	return params, salt, key, nil
}

// utf16Len reports JavaScript's String.prototype.length for the same string.
func utf16Len(value string) int {
	n := 0
	for _, r := range value {
		if r > 0xffff {
			n += 2
			continue
		}
		n++
	}
	return n
}
