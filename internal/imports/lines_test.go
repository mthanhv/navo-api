package imports

import "testing"

func TestNumberVATDate(t *testing.T) {
	for in, want := range map[string]string{"1234.5": "1234.5", "1,234.5": "1234.5", "1.234,5": "1234.5", "1.000.000": "1000000", "1,000,000": "1000000", "12,5": "12.5", "10": "10", "4 850 000": "4850000", "1,000": "1000"} {
		if d, ok := number(in); !ok || d.String() != want {
			t.Errorf("number(%q) = %v %v, want %s", in, d, ok, want)
		}
	}
	for in, want := range map[string]string{"8": "0.08", "8%": "0.08", "0.08": "0.08", "10 %": "0.1", "0": "0"} {
		if d, ok := vatRate(in); !ok || d.String() != want {
			t.Errorf("vatRate(%q) = %v %v, want %s", in, d, ok, want)
		}
	}
	for in, want := range map[string]string{"2026-12-15": "2026-12-15", "15/12/2026": "2026-12-15", "46371": "2026-12-15"} {
		if d, ok := date(in); !ok || d != want {
			t.Errorf("date(%q) = %v %v, want %s", in, d, ok, want)
		}
	}
	if _, ok := number("abc"); ok {
		t.Error("abc is not a number")
	}
}
