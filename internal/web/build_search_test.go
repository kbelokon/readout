package web

import (
	"testing"
	"unicode/utf8"
)

// TestMarkFirstMatchKeepsRuneBoundaries pins the D12 mark split against
// lowering that changes INDIVIDUAL rune widths while preserving the whole
// string's byte length: Ⱥ (2 bytes) lowers to ⱥ (3 bytes) while
// Å (ANGSTROM SIGN, 3 bytes) lowers to å (2 bytes), so a byte
// offset taken from the lowered scan lands MID-RUNE in the original even
// though the whole-string length guard passes. Every split must satisfy
// pre+mark+post == display with all three parts valid UTF-8; a candidate
// occurrence that cannot be applied cleanly degrades to no mark, never to a
// mid-rune split.
func TestMarkFirstMatchKeepsRuneBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		display  string
		words    []string
		wantMark string
	}{
		{
			// The regression: "å" matches the lowered Å at lowered
			// offset 3, but display offset 3 is inside the 3-byte Å.
			// The old whole-string guard accepted it (2+3 == 3+2) and split
			// mid-rune; the per-occurrence guard rejects it -> no mark.
			name:     "misaligned lowering degrades to no mark",
			display:  "\u023A\u212B-cluster",
			words:    []string{"\u00E5"},
			wantMark: "",
		},
		{
			// Same tricky string, but the candidate offset happens to be
			// rune-aligned and fold-equal: the guard ACCEPTS it.
			name:     "aligned occurrence in a shifted string still marks",
			display:  "\u023A\u212B-cluster",
			words:    []string{"cluster"},
			wantMark: "cluster",
		},
		{
			name:     "ascii fast path marks case-insensitively",
			display:  "API-Cluster",
			words:    []string{"clu"},
			wantMark: "Clu",
		},
		{
			// lowerWord ("ⱥ", 3 bytes) differs in length from the word
			// ("Ⱥ", 2 bytes) -> the exact-case fallback finds it.
			name:     "exact-case fallback for width-changing words",
			display:  "x-\u023A-pod",
			words:    []string{"\u023A"},
			wantMark: "\u023A",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pre, mark, post := markFirstMatch(c.display, c.words)
			if pre+mark+post != c.display {
				t.Fatalf("pre+mark+post = %q + %q + %q, want exactly %q", pre, mark, post, c.display)
			}
			for i, part := range []string{pre, mark, post} {
				if !utf8.ValidString(part) {
					t.Fatalf("part %d = %q is not valid UTF-8 (mid-rune split)", i, part)
				}
			}
			if mark != c.wantMark {
				t.Fatalf("mark = %q, want %q", mark, c.wantMark)
			}
		})
	}
}
