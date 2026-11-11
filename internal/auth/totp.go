package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters every authenticator app understands: SHA-1, 6 digits, 30-second steps.
const (
	totpStep   = 30
	totpDigits = 6
	// totpSkew accepts the previous and next step, for phone clocks a little off.
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a random 160-bit secret in base32, as authenticator apps expect.
func newTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return b32.EncodeToString(raw), nil
}

// totpURI is the otpauth:// link an authenticator app opens or scans.
func totpURI(secret, email string) string {
	label := url.PathEscape("Navo:" + email)
	q := url.Values{"secret": {secret}, "issuer": {"Navo"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func hotp(secret []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", v)
}

// verifyTOTP returns the matched time step when code is valid at now (±1 step) and newer
// than lastStep, so the same code never works twice.
func verifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return 0, false
	}
	step := now.Unix() / totpStep
	for d := -totpSkew; d <= totpSkew; d++ {
		s := step + int64(d)
		if s <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s))), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// newRecoveryCodes returns n one-time codes like "k7m2-9xqa" and their hashes.
func newRecoveryCodes(n int) ([]string, [][]byte, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o/1/l/i
	codes, hashes := make([]string, n), make([][]byte, n)
	for i := range codes {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		for j, x := range raw {
			if j == 4 {
				b.WriteByte('-')
			}
			b.WriteByte(alphabet[int(x)%len(alphabet)])
		}
		codes[i] = b.String()
		hashes[i] = hashRecovery(codes[i])
	}
	return codes, hashes, nil
}

func hashRecovery(code string) []byte {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))))
	return sum[:]
}

// CodeAt returns the code an authenticator app shows for secret at t (tests and tooling).
func CodeAt(secret string, t time.Time) string {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}
	return hotp(key, uint64(t.Unix()/totpStep))
}
