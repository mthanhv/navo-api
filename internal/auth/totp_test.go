package auth

import (
	"encoding/base32"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 secret "12345678901234567890", truncated to 6 digits.
func TestTOTPMatchesRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		step, ok := verifyTOTP(secret, want, time.Unix(unix, 0), 0)
		if !ok || step != unix/30 {
			t.Fatalf("t=%d: code %s not accepted (step %d)", unix, want, step)
		}
		if _, ok := verifyTOTP(secret, want, time.Unix(unix, 0), step); ok {
			t.Fatalf("t=%d: the same code must not work twice", unix)
		}
	}
	if _, ok := verifyTOTP(secret, "287082", time.Unix(59+120, 0), 0); ok {
		t.Fatal("a code four steps old is refused")
	}
}

func TestRecoveryCodesAreUniqueAndNormalised(t *testing.T) {
	codes, hashes, err := newRecoveryCodes(8)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 9 || c[4] != '-' || seen[c] {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
		if string(hashRecovery(" "+c[:4]+" "+c[4:]+" ")) != string(hashes[i]) && string(hashRecovery(c)) != string(hashes[i]) {
			t.Fatalf("hash mismatch for %q", c)
		}
	}
	if string(hashRecovery("ABCD-EFGH")) != string(hashRecovery("abcd-efgh")) {
		t.Fatal("recovery codes are case-insensitive")
	}
}
