package yamlview

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"sigs.k8s.io/yaml"
)

// podObject is a representative resource object (the shape the resource-view
// marshals): mixed scalar types, nested maps, a list of maps, a quoted
// timestamp, and a block-scalar-eligible multiline string.
func podObject() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":              "nginx",
			"namespace":         "default",
			"creationTimestamp": "2024-03-01T10:00:00Z",
			"labels":            map[string]any{"app": "nginx"},
		},
		"spec": map[string]any{
			"restartPolicy":      "Always",
			"enableServiceLinks": true,
			"containers": []any{
				map[string]any{
					"name":  "nginx",
					"image": "nginx:1.25",
					"ports": []any{map[string]any{"containerPort": int64(80)}},
				},
			},
		},
		"status": map[string]any{
			"phase": "Running",
			"podIP": "10.0.0.1",
		},
	}
}

// TestMarshalParseEquivalence pins the serialization invariant: the YAML that
// Marshal produces re-parses (via sigs.k8s.io/yaml, the same engine) to a value
// DEEP EQUAL to the input. The value-level round-trip is the invariant.
func TestMarshalParseEquivalence(t *testing.T) {
	in := podObject()
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, data)
	}
	// sigs.k8s.io/yaml routes through JSON, so an int64 input scalar comes back
	// as float64; normalise the input the same way for a fair deep-equal.
	want := jsonNormalise(t, in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\nwant %#v\ngot  %#v\nyaml:\n%s", want, got, data)
	}
}

// TestMarshalTopLevelKeysPresent: every top-level key survives serialization and
// keys are emitted in deterministic sorted order (sigs.k8s.io/yaml behaviour).
func TestMarshalTopLevelKeysPresent(t *testing.T) {
	data, err := Marshal(podObject())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, key := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if !strings.Contains(text, key+":") {
			t.Fatalf("top-level key %q missing from output:\n%s", key, text)
		}
	}
	// Deterministic sorted-key order: apiVersion < kind < metadata < spec < status.
	order := []string{"apiVersion:", "kind:", "metadata:", "spec:", "status:"}
	last := -1
	for _, k := range order {
		idx := strings.Index(text, "\n"+k)
		if k == "apiVersion:" {
			idx = strings.Index(text, k)
		}
		if idx <= last {
			t.Fatalf("top-level key %q not in sorted position (idx=%d, last=%d):\n%s", k, idx, last, text)
		}
		last = idx
	}
}

// TestMarshalDeterministic: same input -> identical bytes (sorted keys, no map
// iteration nondeterminism). Guards the pane/download against flapping.
func TestMarshalDeterministic(t *testing.T) {
	a, err := Marshal(podObject())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		b, err := Marshal(podObject())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("non-deterministic marshal:\nA:\n%s\nB:\n%s", a, b)
		}
	}
}

// maskSecretLike applies the SAME transformation web.maskSecret applies before
// serialization (mask every data value, wipe annotations). yamlview is pure and
// does not own the mask helper; this asserts the SERIALIZE step is leak-free for
// a masked object -- no raw secret bytes survive marshalling.
func maskSecretLike(obj map[string]any, hidden string) {
	if data, ok := obj["data"].(map[string]any); ok {
		for k := range data {
			data[k] = hidden
		}
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["annotations"] = map[string]any{"annotations-hidden": "by-readout"}
}

// TestMarshalMaskedSecretHasNoRawBytes: after masking, neither the base64 data
// values nor the original last-applied-configuration annotation appear in the
// marshalled output -- only the hidden sentinel.
func TestMarshalMaskedSecretHasNoRawBytes(t *testing.T) {
	const hidden = "**SECRET-CONTENT-HIDDEN-BY-READOUT**"
	// rawTokenValue is a base64 secret blob (decodes to a super-secret token).
	const rawTokenValue = "c3VwZXItc2VjcmV0LXRva2VuLXZhbHVl"
	const rawAnnotationSecret = "kubectl.kubernetes.io/last-applied-configuration"
	const rawAnnotationPayload = `{"apiVersion":"v1","data":{"token":"c3VwZXI..."}}`

	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": "creds",
			"annotations": map[string]any{
				rawAnnotationSecret: rawAnnotationPayload,
				"another":           "visible-but-wiped",
			},
		},
		"data": map[string]any{
			"token":    rawTokenValue,
			"password": "cGFzc3dvcmQ=",
		},
	}
	maskSecretLike(secret, hidden)

	data, err := Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	if !strings.Contains(text, hidden) {
		t.Fatalf("masked data value sentinel missing:\n%s", text)
	}
	for _, raw := range []string{rawTokenValue, "cGFzc3dvcmQ=", rawAnnotationSecret, rawAnnotationPayload, "visible-but-wiped"} {
		if strings.Contains(text, raw) {
			t.Fatalf("raw secret byte %q leaked into marshalled output:\n%s", raw, text)
		}
	}
	// The wipe annotation marker IS expected.
	if !strings.Contains(text, "annotations-hidden") {
		t.Fatalf("annotation wipe marker missing:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// Highlight: the Pygments-DOM contract readout.js requires.
// ---------------------------------------------------------------------------

func parseDoc(t *testing.T, htmlStr string) *goquery.Document {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		t.Fatalf("parse highlight HTML: %v", err)
	}
	return doc
}

// TestHighlightDOMContract pins the exact structure the four JS consumers and
// the Playwright selectors hard-code (strategy (a)): the highlight table with
// linenos + code cells, code-cell line spans id="yaml-<prefix>line-N" as DIRECT
// children of <pre>, matching gutter anchors href="#<prefix>line-N", and the
// td.code textContent reproducing the raw YAML (the copy contract).
func TestHighlightDOMContract(t *testing.T) {
	data, err := Marshal(podObject())
	if err != nil {
		t.Fatal(err)
	}
	out := Highlight(string(data), "", nil)
	doc := parseDoc(t, out)

	if doc.Find("div.highlight table.highlighttable").Length() != 1 {
		t.Fatalf("missing div.highlight > table.highlighttable:\n%s", out)
	}
	if doc.Find("table.highlighttable td.linenos div.linenodiv pre").Length() != 1 {
		t.Fatalf("missing gutter td.linenos > div.linenodiv > pre")
	}
	if doc.Find("table.highlighttable td.code pre").Length() != 1 {
		t.Fatalf("missing code td.code > pre")
	}

	// Line spans: direct children of td.code pre, id="yaml-line-N" (empty prefix),
	// in 1..N order. buildYamlFolds filters pre.children to SPANs whose id has
	// "line-"; the highlight consumer getElementById('yaml-'+frag).
	lineSpans := doc.Find("td.code pre > span[id]")
	srcLines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if lineSpans.Length() != len(srcLines) {
		t.Fatalf("code line span count = %d, want %d (one per source line)", lineSpans.Length(), len(srcLines))
	}
	lineSpans.Each(func(i int, sel *goquery.Selection) {
		wantID := "yaml-line-" + itoa(i+1)
		if id, _ := sel.Attr("id"); id != wantID {
			t.Fatalf("line span %d id = %q, want %q", i+1, id, wantID)
		}
		// Inner bare anchor: <a id="line-N" name="line-N">.
		a := sel.Find("a").First()
		if aid, _ := a.Attr("id"); aid != "line-"+itoa(i+1) {
			t.Fatalf("line %d inner anchor id = %q, want %q", i+1, aid, "line-"+itoa(i+1))
		}
		if an, _ := a.Attr("name"); an != "line-"+itoa(i+1) {
			t.Fatalf("line %d inner anchor name = %q", i+1, an)
		}
	})

	// Gutter anchors target the inner anchors: href="#line-N".
	for i := range srcLines {
		sel := doc.Find(`td.linenos a[href="#line-` + itoa(i+1) + `"]`)
		if sel.Length() != 1 {
			t.Fatalf("gutter missing a[href=\"#line-%d\"]", i+1)
		}
	}

	// Copy contract: td.code textContent == raw YAML (each line span's
	// textContent is the source line + trailing newline; the empty <a> adds
	// nothing). Reconstruct and compare to the marshalled YAML.
	codeText := doc.Find("td.code pre").Text()
	if codeText != string(data) {
		t.Fatalf("td.code textContent != raw YAML\n--- got ---\n%q\n--- want ---\n%q", codeText, string(data))
	}
}

// TestHighlightTokenClasses verifies the token-type -> Pygments-class map paints
// the spans with classes the baked .highlight CSS palette colours: keys (nt),
// punctuation (p), whitespace (w), plain scalars (l), numbers (m), and a quoted
// timestamp string (s2). These are a subset of the map; the rendered colours
// rely on these class names matching readout.css.
func TestHighlightTokenClasses(t *testing.T) {
	data, err := Marshal(podObject())
	if err != nil {
		t.Fatal(err)
	}
	out := Highlight(string(data), "", nil)
	doc := parseDoc(t, out)

	codeClasses := map[string]bool{}
	doc.Find("td.code pre span[id] span[class]").Each(func(_ int, s *goquery.Selection) {
		if c, ok := s.Attr("class"); ok {
			codeClasses[c] = true
		}
	})
	for _, want := range []string{"nt", "p", "w", "l", "m", "s2"} {
		if !codeClasses[want] {
			t.Fatalf("expected a token span with class %q; got classes %v\n%s", want, keys(codeClasses), out)
		}
	}

	// Line 1 is "apiVersion: v1": key span nt + value span l.
	line1 := doc.Find("td.code pre > span#yaml-line-1")
	if c, _ := line1.Find("span.nt").First().Attr("class"); c != "nt" {
		t.Fatalf("line 1 missing nt key span")
	}
	if !strings.Contains(line1.Text(), "apiVersion") {
		t.Fatalf("line 1 text = %q, want apiVersion", line1.Text())
	}
}

// TestHighlightAnchorPrefix: per-key section cards use a "<key>-" prefix; the id
// scheme becomes yaml-<prefix>line-N and gutter #<prefix>line-N. (Pinned for the
// per-section copy/fold to address the right cell.)
func TestHighlightAnchorPrefix(t *testing.T) {
	data, err := Marshal(map[string]any{"replicas": int64(3), "selector": map[string]any{"app": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	out := Highlight(string(data), "spec-", nil)
	doc := parseDoc(t, out)
	if doc.Find("td.code pre > span#yaml-spec-line-1").Length() != 1 {
		t.Fatalf("expected span#yaml-spec-line-1 for prefixed card:\n%s", out)
	}
	if doc.Find(`td.linenos a[href="#spec-line-1"]`).Length() != 1 {
		t.Fatalf("expected gutter a[href=\"#spec-line-1\"]")
	}
	if a, _ := doc.Find("td.code pre > span#yaml-spec-line-1 a").First().Attr("id"); a != "spec-line-1" {
		t.Fatalf("inner anchor id = %q, want spec-line-1", a)
	}
}

// TestHighlightTimestampCallback: the linkTimestamps transform is applied per
// rendered code line (used by web to inject <a> timestamp links). yamlview stays
// pure -- it knows nothing about clusters/config, only this callback.
func TestHighlightTimestampCallback(t *testing.T) {
	data, err := Marshal(map[string]any{"creationTimestamp": "2024-03-01T10:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	out := Highlight(string(data), "", func(line string) string {
		called++
		return strings.ReplaceAll(line, "2024-03-01T10:00:00Z", `<a href="/t">2024-03-01T10:00:00Z</a>`)
	})
	if called == 0 {
		t.Fatalf("linkTimestamps callback never invoked")
	}
	if !strings.Contains(out, `<a href="/t">2024-03-01T10:00:00Z</a>`) {
		t.Fatalf("timestamp link not injected:\n%s", out)
	}
}

// TestHighlightBlockScalarFaithful: a block scalar (multiline value, |) keeps
// every body line as its own line span and the td.code textContent stays equal
// to the raw YAML -- the copy/fold contract must not drop block-scalar lines.
func TestHighlightBlockScalarFaithful(t *testing.T) {
	data, err := Marshal(map[string]any{
		"ca.crt": "-----BEGIN CERTIFICATE-----\nABC DEF\n-----END CERTIFICATE-----\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := Highlight(string(data), "", nil)
	doc := parseDoc(t, out)
	srcLines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if got := doc.Find("td.code pre > span[id]").Length(); got != len(srcLines) {
		t.Fatalf("block-scalar line span count = %d, want %d", got, len(srcLines))
	}
	if codeText := doc.Find("td.code pre").Text(); codeText != string(data) {
		t.Fatalf("block-scalar td.code textContent != raw YAML\n got %q\nwant %q", codeText, string(data))
	}
}

// TestHighlightEscapesHTML: a value containing HTML metacharacters is escaped in
// the rendered spans (no raw-tag injection), while the td.code textContent still
// decodes back to the exact source.
func TestHighlightEscapesHTML(t *testing.T) {
	data, err := Marshal(map[string]any{"note": "<script>&\"'</script>"})
	if err != nil {
		t.Fatal(err)
	}
	out := Highlight(string(data), "", nil)
	if strings.Contains(out, "<script>") {
		t.Fatalf("unescaped <script> in highlight output:\n%s", out)
	}
	doc := parseDoc(t, out)
	if codeText := doc.Find("td.code pre").Text(); codeText != string(data) {
		t.Fatalf("escaped td.code textContent != raw YAML\n got %q\nwant %q", codeText, string(data))
	}
}

// --- small local helpers (no external deps) ---

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// jsonNormalise pushes value through Marshal+Unmarshal so int64 etc. land in the
// same float64 shape sigs.k8s.io/yaml produces, for a fair round-trip compare.
func jsonNormalise(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
