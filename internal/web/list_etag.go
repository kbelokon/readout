package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/kbelokon/readout/internal/web/templates"
)

// resourceListETagDomain separates this preimage from unrelated SHA-256 uses.
// Renderer changes are invalidated automatically by resourceListRendererFingerprint.
const resourceListETagDomain = "readout.resource-list.partial\x00"

// resourceListETag returns a weak validator for the semantic ResourceTable
// representation. Request duration is intentionally excluded: it is diagnostic
// timing, not resource state, so a 304 keeps the last rendered timing. The stale
// banner flag is also excluded because ResourceTable never renders it (the full
// page wrapper does).
func resourceListETag(data *templates.ListData) (string, error) {
	return resourceListETagWithRendererFingerprint(data, resourceListRendererFingerprint())
}

func resourceListETagWithRendererFingerprint(data *templates.ListData, renderer [sha256.Size]byte) (string, error) {
	semantic := resourceStateListData(data)
	semantic.DurationSeconds = 0
	semantic.ShowStaleBanner = false

	payload, err := json.Marshal(semantic)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(resourceListETagDomain))
	_, _ = hash.Write(renderer[:])
	_, _ = hash.Write(payload)
	digest := base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
	return `W/"ro-list-v1-` + digest + `"`, nil
}

// setResourceListValidatorHeaders applies the metadata contract shared by a
// successful table fragment and its bodyless 304 response. Cache-Control
// deliberately disables ambient browser/shared-cache reuse: readout.js owns the
// validator and sends it only for an exact container refresh.
func setResourceListValidatorHeaders(header http.Header, etag string) {
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "private, no-store")
	header.Set("ETag", etag)
	addVary(header, "Accept-Encoding")
}

// isConditionalListRefresh is the server-side belt around the app-managed
// validator path. User sort/filter requests must remain unconditional even if
// an If-None-Match header is accidentally injected, because a 304 would suppress
// their normal body swap/history behavior.
func isConditionalListRefresh(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("RO-No-Push") == "true"
}

// ifNoneMatch reports whether values contain current under RFC weak comparison.
// Header.Values is required: intermediaries may preserve repeated fields instead
// of joining them. Malformed input fails open to a 200 response; a syntactically
// valid match is returned only after the complete combined field was parsed.
func ifNoneMatch(values []string, current string) bool {
	if len(values) == 0 {
		return false
	}
	value := textproto.TrimString(strings.Join(values, ","))
	if value == "*" {
		return true
	}

	matched := false
	sawTag := false
	for {
		value = textproto.TrimString(value)
		// HTTP list syntax permits recipients to ignore empty list elements.
		for strings.HasPrefix(value, ",") {
			value = textproto.TrimString(value[1:])
		}
		if value == "" {
			return sawTag && matched
		}
		// If-None-Match is either a standalone wildcard or a list of entity tags;
		// a wildcard embedded in that list is malformed.
		if value[0] == '*' {
			return false
		}

		etag, remain := scanListETag(value)
		if etag == "" {
			return false
		}
		sawTag = true
		if weakETagMatch(etag, current) {
			matched = true
		}
		remain = textproto.TrimString(remain)
		if remain == "" {
			return matched
		}
		if remain[0] != ',' {
			return false
		}
		value = remain[1:]
	}
}

// scanListETag consumes one syntactically-valid entity-tag without splitting on
// commas (a comma is legal inside the opaque quoted value). It mirrors net/http's
// internal scanETag grammar: optional uppercase W/, then an RFC entity-tag.
func scanListETag(value string) (etag, remain string) {
	value = textproto.TrimString(value)
	start := 0
	if strings.HasPrefix(value, "W/") {
		start = 2
	}
	if len(value[start:]) < 2 || value[start] != '"' {
		return "", ""
	}
	for i := start + 1; i < len(value); i++ {
		c := value[i]
		switch {
		case c == 0x21 || c >= 0x23 && c <= 0x7e || c >= 0x80:
		case c == '"':
			return value[:i+1], value[i+1:]
		default:
			return "", ""
		}
	}
	return "", ""
}

func weakETagMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}
