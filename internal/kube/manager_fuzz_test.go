package kube

import (
	"regexp"
	"testing"
	"unicode/utf8"
)

// allowedSanitizedName matches a fully-sanitized cluster name: only the
// characters SanitizeClusterName is allowed to leave intact. Every invalid
// rune is mapped to ":" (manager.go), so a sanitized name contains nothing
// outside this class.
var allowedSanitizedName = regexp.MustCompile(`^[a-zA-Z0-9:_.-]*$`)

// FuzzSanitizeClusterName asserts the invariants of readout's ACTUAL
// SanitizeClusterName, which replaces every character outside
// [a-zA-Z0-9:_.-] with ":" and does NOT cap length or strip to empty.
//
// The invariants below are deliberately scoped to what that replacement
// guarantees -- they are NOT Headlamp's (no 50-char cap, no strip-to-empty,
// no "never grows" byte-length claim). They hold for every input including
// multibyte and invalid-UTF-8 bytes:
//
//  1. charset: the output contains only allowed characters.
//  2. valid UTF-8: the output is always valid UTF-8 (each replacement emits the
//     single ASCII byte ':', and Go's regexp matches the invalid set per-rune,
//     so a multibyte invalid rune becomes one ':' rather than several).
//  3. idempotent: sanitizing an already-sanitized name is a no-op.
func FuzzSanitizeClusterName(f *testing.F) {
	// Seed cases (mirrored by the testdata/fuzz corpus): a plain name, special
	// characters, slashes, a colon-form, a unicode name, an empty string, and a
	// colon-laden name.
	seeds := []string{
		"plain-cluster",
		"my-cluster@#$%",
		"team/prod",
		"team:prod",
		"unicode-日本語-cluster",
		"",
		"::a::b::c::",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got := SanitizeClusterName(name)

		// Invariant 1: only allowed characters survive.
		if !allowedSanitizedName.MatchString(got) {
			t.Fatalf("output contains disallowed characters: input=%q output=%q", name, got)
		}

		// Invariant 2: the output is valid UTF-8 regardless of input bytes.
		if !utf8.ValidString(got) {
			t.Fatalf("output is not valid UTF-8: input=%q output=%q", name, got)
		}

		// Invariant 3: idempotent.
		if again := SanitizeClusterName(got); again != got {
			t.Fatalf("not idempotent: input=%q once=%q twice=%q", name, got, again)
		}
	})
}
