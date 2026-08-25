package web

import (
	"mime"
	"strings"
	"testing"
)

func TestSafeSpreadsheetCell(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"hyperlink formula", "=HYPERLINK(\"http://evil\",\"x\")", "'=HYPERLINK(\"http://evil\",\"x\")"},
		{"at command", "@cmd", "'@cmd"},
		{"leading minus", "-1+1", "'-1+1"},
		{"leading plus", "+1", "'+1"},
		{"leading tab", "\tx", "'\tx"},
		{"leading cr", "\rx", "'\rx"},
		{"leading lf", "\nx", "'\nx"},
		{"embedded newline only", "ok\n=evil", "ok\n=evil"},
		{"plain text", "Running", "Running"},
		{"empty", "", ""},
		{"numeric without sign", "1023", "1023"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeSpreadsheetCell(tc.in); got != tc.want {
				t.Fatalf("safeSpreadsheetCell(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSafeAttachmentFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "pods.tsv", "pods.tsv"},
		{"slash", "a/b/c.tsv", "a_b_c.tsv"},
		{"backslash", `a\b.tsv`, "a_b.tsv"},
		{"both separators", `a/b\c.tsv`, "a_b_c.tsv"},
		{"strip crlf", "evil\r\nSet-Cookie: x", "evilSet-Cookie: x"},
		{"strip control", "a\tb.tsv", "ab.tsv"},
		{"strip last C0 control", "a\x1fb.tsv", "ab.tsv"},
		{"keep first printable ASCII", "a b.tsv", "a b.tsv"},
		{"strip delete", "a\x7fb.tsv", "ab.tsv"},
		{"strip first C1 control", "a\u0080b.tsv", "ab.tsv"},
		{"strip last C1 control", "a\u009fb.tsv", "ab.tsv"},
		{"keep first non-control after C1", "a\u00a0b.tsv", "a\u00a0b.tsv"},
		{"unicode kept", "néinstüd.tsv", "néinstüd.tsv"},
		{"empty fallback", "", "download"},
		{"all-control fallback", "\r\n\t", "download"},
		{"all-C1 fallback", "\u0080\u0085\u009f", "download"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeAttachmentFilename(tc.in); got != tc.want {
				t.Fatalf("safeAttachmentFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAttachmentDispositionParsesBack proves the header is built via
// mime.FormatMediaType: every produced value round-trips through
// mime.ParseMediaType back to an `attachment` disposition with the sanitized
// filename, and contains no raw CR/LF that could split the header.
func TestAttachmentDispositionParsesBack(t *testing.T) {
	inputs := []struct {
		in   string
		want string
	}{
		{in: "pods.tsv", want: "pods.tsv"},
		{in: "clusters_dev_namespaces_kube-system_pods.tsv", want: "clusters_dev_namespaces_kube-system_pods.tsv"},
		{in: "néinstüd.yaml", want: "néinstüd.yaml"},
		{in: "evil\r\nSet-Cookie: x.txt", want: "evilSet-Cookie: x.txt"},
		{in: "C1\u0085control.tsv", want: "C1control.tsv"},
		{in: ".tsv", want: ".tsv"},
		{in: "a/b/c_bulk.yaml", want: "a_b_c_bulk.yaml"},
	}
	for _, tc := range inputs {
		header := attachmentDisposition(tc.in)
		if strings.ContainsAny(header, "\r\n") {
			t.Fatalf("attachmentDisposition(%q) leaked CR/LF: %q", tc.in, header)
		}
		disp, params, err := mime.ParseMediaType(header)
		if err != nil {
			t.Fatalf("attachmentDisposition(%q) = %q did not parse: %v", tc.in, header, err)
		}
		if disp != "attachment" {
			t.Fatalf("disposition = %q, want attachment (input %q)", disp, tc.in)
		}
		if params["filename"] != tc.want {
			t.Fatalf("filename param = %q, want %q (input %q)", params["filename"], tc.want, tc.in)
		}
	}
}
