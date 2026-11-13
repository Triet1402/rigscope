package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, PasswordHashPrefix) {
		t.Fatalf("hash %q does not use the fixed parameters", hash)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(correct) = %v, %v", ok, err)
	}
	for _, wrong := range []string{"", "correct horse battery stapl", "correct horse battery staple ", "密码"} {
		ok, err := VerifyPassword(wrong, hash)
		if err != nil {
			t.Fatalf("VerifyPassword(%q): %v", wrong, err)
		}
		if ok {
			t.Fatalf("VerifyPassword(%q) accepted a wrong password", wrong)
		}
	}
}

func TestHashPasswordFreshSalt(t *testing.T) {
	first, err := HashPassword("same password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword("same password")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("identical passwords produced identical hashes")
	}
}

func TestHashPasswordBounds(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("empty password accepted")
	}
	if _, err := HashPassword(strings.Repeat("x", MaxPasswordChars+1)); err == nil {
		t.Fatal("overlong password accepted")
	}
}

func TestVerifyRejectsAbsurdStoredParameters(t *testing.T) {
	cases := []string{
		"",
		"plaintext-password",
		"scrypt$16384$8$1$00$00",
		"$argon2id$v=19$m=99999999,t=3,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=32768,t=99,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=32768,t=3,p=0$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=32768,t=3,p=2$%%%$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2i$v=19$m=32768,t=3,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=16$m=32768,t=3,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
	}
	for _, encoded := range cases {
		ok, err := VerifyPassword("anything", encoded)
		if ok {
			t.Fatalf("accepted %q", encoded)
		}
		if err == nil {
			t.Fatalf("accepted stored parameters in %q without an error", encoded)
		}
		if !errors.Is(err, ErrInvalidHash) {
			t.Fatalf("VerifyPassword(%q) error = %v, want ErrInvalidHash", encoded, err)
		}
	}
}

func TestVerifyRefusesWorkBudget(t *testing.T) {
	// Each bound passes on its own, but time*memory exceeds the budget.
	encoded := "$argon2id$v=19$m=262144,t=10,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5"
	if _, err := VerifyPassword("x", encoded); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("error = %v, want ErrInvalidHash", err)
	}
}
