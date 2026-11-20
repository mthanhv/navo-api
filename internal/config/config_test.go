package config

import "testing"

func TestSMTPAllowInsecure(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres:///navo_test")
	t.Setenv("NAVO_SMTP_HOST", "mailpit")
	t.Setenv("NAVO_MAIL_FROM", "navo@example.test")
	for _, tc := range []struct {
		value   string
		want    bool
		invalid bool
	}{
		{"", false, false}, {"true", true, false}, {"false", false, false}, {"typo", false, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("NAVO_SMTP_ALLOW_INSECURE", tc.value)
			cfg, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("Load error = %v", err)
			}
			if !tc.invalid && cfg.SMTP.AllowInsecure != tc.want {
				t.Fatalf("AllowInsecure = %v", cfg.SMTP.AllowInsecure)
			}
		})
	}
}
