package workflow

import (
	"strings"
	"testing"
	"unicode"
)

func TestGenerateStorageBoxPasswordMeetsPolicy(t *testing.T) {
	password := generateStorageBoxPassword()
	if len(password) < 12 {
		t.Fatalf("password too short: %d", len(password))
	}

	var hasUpper bool
	var hasLower bool
	var hasDigit bool
	var hasSpecial bool
	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		case strings.ContainsRune("!@#$%^&*()-_=+[]{}:,.?", r):
			hasSpecial = true
		}
	}

	if !hasUpper || !hasLower || !hasDigit || !hasSpecial {
		t.Fatalf("password does not meet complexity policy: %q", password)
	}
}
