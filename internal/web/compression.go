package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/klauspost/compress/gzhttp"
)

const minCompressSize = 1024

var dynamicCompressionWrapper = mustCompressionWrapper()

func mustCompressionWrapper() func(http.Handler) http.HandlerFunc {
	wrapper, err := gzhttp.NewWrapper(
		gzhttp.MinSize(minCompressSize),
		gzhttp.EnableZstd(false),
		gzhttp.ContentTypes([]string{
			"text/html",
			"text/plain",
			"text/css",
			"text/javascript",
			"text/tab-separated-values",
			"text/vnd.yaml",
			"text/xml",
			"text/yaml",
			"application/javascript",
			"application/json",
			"application/xml",
			"application/yaml",
			"application/x-yaml",
			"image/svg+xml",
		}),
	)
	if err != nil {
		panic("configure response compression: " + err.Error())
	}
	return wrapper
}

// compressResponses delegates finite dynamic response compression to gzhttp.
// Assets have their own immutable representation policy, and Live streams must
// retain the original writer and per-frame flush behavior, so both bypass the
// wrapper by route.
func compressResponses(next http.Handler) http.Handler {
	compressed := dynamicCompressionWrapper(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipCompressionRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodHead {
			// gzhttp deliberately selects identity for HEAD. Run its representation
			// negotiation as GET while retaining a bodyless wire response so HEAD
			// reports the same Content-Encoding and selected length as GET.
			head := &headMetadataResponseWriter{ResponseWriter: w}
			get := r.Clone(r.Context())
			get.Method = http.MethodGet
			compressed(&weakETagResponseWriter{ResponseWriter: head}, get)
			// ServeMux records the matched route on the request IT was handed,
			// which is the clone. Copy it back so the outer metrics/access-log
			// middleware labels a HEAD with its route instead of __unmatched__.
			r.Pattern = get.Pattern
			head.finish()
			return
		}
		compressed(&weakETagResponseWriter{ResponseWriter: w}, r)
	})
}

func skipCompressionRequest(r *http.Request) bool {
	return r.Method == http.MethodConnect ||
		strings.HasPrefix(r.URL.Path, "/assets/") ||
		strings.HasSuffix(r.URL.Path, "/_stream") ||
		r.Header.Get("Upgrade") != "" ||
		headerHasToken(r.Header.Values("Connection"), "upgrade")
}

// Compression selects a transfer representation, so a strong validator for
// the handler bytes cannot remain strong across that selection. Keep the old
// weak-validator contract in a tiny writer while gzhttp owns all buffering,
// negotiation, compression, and finalization behavior.
type weakETagResponseWriter struct {
	http.ResponseWriter
}

// headMetadataResponseWriter counts the selected GET representation without
// forwarding its body. It delays the final status until gzhttp has closed its
// encoder, when the compressed byte length and representation headers are
// authoritative. No body bytes are retained.
type headMetadataResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// Unwrap keeps http.ResponseController able to reach the real connection from
// behind the HEAD metadata writer.
func (w *headMetadataResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *headMetadataResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
	}
}

func (w *headMetadataResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.bytes += int64(len(p))
	return len(p), nil
}

func (w *headMetadataResponseWriter) finish() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	header := w.Header()
	if !responseStatusHasBody(w.status) {
		header.Del("Content-Length")
	} else if header.Get("Content-Length") == "" && !responseHasTrailers(header) {
		header.Set("Content-Length", strconv.FormatInt(w.bytes, 10))
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func responseStatusHasBody(status int) bool {
	return (status < 100 || status >= 200) &&
		status != http.StatusNoContent &&
		status != http.StatusResetContent &&
		status != http.StatusNotModified
}

func responseHasTrailers(header http.Header) bool {
	if len(header.Values("Trailer")) != 0 {
		return true
	}
	for name := range header {
		if strings.HasPrefix(name, http.TrailerPrefix) {
			return true
		}
	}
	return false
}

func (w *weakETagResponseWriter) WriteHeader(status int) {
	weakenStrongETag(w.Header())
	w.ResponseWriter.WriteHeader(status)
}

func (w *weakETagResponseWriter) Write(p []byte) (int, error) {
	weakenStrongETag(w.Header())
	return w.ResponseWriter.Write(p)
}

func (w *weakETagResponseWriter) Flush() {
	weakenStrongETag(w.Header())
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *weakETagResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func weakenStrongETag(header http.Header) {
	value := strings.TrimSpace(header.Get("ETag"))
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return
	}
	for i := 1; i < len(value)-1; i++ {
		c := value[i]
		if c == 0x21 || c >= 0x23 && c <= 0x7e || c >= 0x80 {
			continue
		}
		return
	}
	header.Set("ETag", "W/"+value)
}

func addVary(header http.Header, token string) {
	for _, value := range header.Values("Vary") {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "*" || strings.EqualFold(item, token) {
				return
			}
		}
	}
	header.Add("Vary", token)
}

func headerHasToken(values []string, token string) bool {
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			name := item
			if before, _, ok := strings.Cut(item, "="); ok {
				name = before
			}
			if strings.EqualFold(strings.TrimSpace(name), token) {
				return true
			}
		}
	}
	return false
}
