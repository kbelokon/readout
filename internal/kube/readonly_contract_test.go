package kube

import (
	"reflect"
	"regexp"
	"testing"
)

// mutatingVerb matches the leading verb of any method name that would let the
// kube gateway change cluster state. The gateway is read-only by construction:
// it exposes discovery, list, get, table, watch and logs, but never a verb that
// writes. Pinning the absence of these verbs makes the read-only guarantee
// survive any future method addition -- the moment a mutating method appears on
// the client, this regression fails.
var mutatingVerb = regexp.MustCompile(`^(Create|Update|Patch|Delete|Apply)`)

// TestReadOnlyClientExposesNoMutatingVerb walks the exported method set of the
// concrete *Client pointer type and asserts no method name begins with a
// mutating verb. It reflects over the real type (not an interface) so a method
// added to the client tomorrow is covered automatically without touching this
// test.
func TestReadOnlyClientExposesNoMutatingVerb(t *testing.T) {
	clientType := reflect.TypeOf(&Client{})

	if clientType.NumMethod() == 0 {
		t.Fatal("*Client exposes no exported methods -- the reflection pin would be vacuously green; the method set must be non-empty")
	}

	for i := 0; i < clientType.NumMethod(); i++ {
		name := clientType.Method(i).Name
		if mutatingVerb.MatchString(name) {
			t.Errorf("*Client exposes a mutating method %q -- the kube gateway must stay read-only", name)
		}
	}
}
