package auth

import "testing"

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Error("wrong password accepted")
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Error("identical passwords produced identical hashes; salt is not random")
	}
}

// TestVerifyRejectsUnusableHashes covers the case that matters most: a user row
// with no password set must not be loggable-into by any input, including "".
func TestVerifyRejectsUnusableHashes(t *testing.T) {
	for _, hash := range []string{
		"",
		"not-a-hash",
		"$argon2id$v=19$m=65536,t=3,p=2$badbase64!$x",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0$aGFzaA",
		"$argon2id$v=1$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0$aGFzaA",
	} {
		if VerifyPassword(hash, "") {
			t.Errorf("empty password accepted against hash %q", hash)
		}
		if VerifyPassword(hash, "anything") {
			t.Errorf("password accepted against unusable hash %q", hash)
		}
	}
}
