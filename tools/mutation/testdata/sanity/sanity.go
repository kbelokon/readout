// Package sanity is a mutation-runner canary, not application code.
package sanity

// KilledBoundary is asserted at the exact boundary. Its mutants must be killed.
func KilledBoundary(value int) bool {
	return value >= 10
}

// LivedBoundary is executed but deliberately not asserted, guaranteeing that
// the canary also contains covered mutants which must live.
func LivedBoundary(value int) bool {
	return value >= 10
}

// CompileBoundary creates one deliberately compile-invalid arithmetic mutant:
// replacing string addition with subtraction must be classified NOT VIABLE,
// never inflated into KILLED.
func CompileBoundary(left, right string) string {
	return left + right
}
