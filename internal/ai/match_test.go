package ai

import (
	"testing"

	"github.com/google/uuid"
)

func TestRankIgnoresAccentsAndCase(t *testing.T) {
	all := []Candidate{{ID: uuid.New(), Name: "Van cầu DN50 PN40"}, {ID: uuid.New(), Name: "Van cầu DN50 PN16"}, {ID: uuid.New(), Name: "Bơm định lượng"}}
	r := rank("VAN CAU DN50 PN40", all, func(c Candidate) string { return c.Name })
	if len(r) < 2 || r[0].c.Name != "Van cầu DN50 PN40" || r[0].score != 1 {
		t.Fatalf("exact name (accents ignored) ranks first with score 1: %+v", r)
	}
	if r[1].score >= 1 {
		t.Fatal("a similar name is only a candidate")
	}
	if len(rank("Gioăng xoắn", all, func(c Candidate) string { return c.Name })) != 0 {
		t.Fatal("unrelated names are not offered")
	}
}

func TestDisabledWithoutKey(t *testing.T) {
	if New("", "claude-opus-5-5").Enabled() {
		t.Fatal("no key, no AI")
	}
	if _, err := New("", "x").Extract(t.Context(), "application/pdf", []byte("%PDF")); err == nil {
		t.Fatal("disabled extractor refuses")
	}
}
