package numbering

import "testing"

func TestNumbers(t *testing.T) {
	cases := map[string]string{
		ContractNumber(2026, 142):            "SC-26-0142",
		ContractNumber(2025, 7):              "SC-25-0007",
		ChildNumber("PO", "SC-26-0142", 1):   "PO-26-0142-01",
		ChildNumber("DO", "SC-26-0142", 12):  "DO-26-0142-12",
		ChildNumber("GR", "SC-26-0142", 105): "GR-26-0142-105",
		DraftNumber(31):                      "DRAFT-0031",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}
