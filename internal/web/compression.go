package web

import (
	"compress/gzip"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minCompressSize keeps tiny responses (health checks, redirects and short
// errors) out of the gzip path. The response writer buffers at most this much
// before it either starts gzip or commits the original representation.
const minCompressSize = 1024

var gzipWriters = sync.Pool{
	New: func() any {
		return gzip.NewWriter(io.Discard)
	},
}

// compressResponses gzip-compresses sufficiently large, textual, finite
// responses when the client accepts gzip. Known streaming/connection-oriented
// requests are passed the original ResponseWriter so their optional interfaces
// and flush timing are completely unchanged.
func compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipCompressionRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		destination := w
		var head *headResponseWriter
		if r.Method == http.MethodHead {
			// A HEAD handler still writes the would-be GET body. Feed those bytes
			// through the normal threshold and representation selection so its
			// metadata matches GET, but never forward body bytes to the wire.
			head = &headResponseWriter{ResponseWriter: w}
			destination = head
		}
		cw := &compressionWriter{
			ResponseWriter:     destination,
			acceptsGzip:        acceptsEncoding(r.Header.Values("Accept-Encoding"), "gzip"),
			requestNoTransform: hasDirective(r.Header.Values("Cache-Control"), "no-transform"),
		}
		next.ServeHTTP(cw, r)
		finishErr := cw.finish()
		if head != nil {
			head.finish()
		}
		if finishErr != nil {
			// The response may already be committed, so a compression failure
			// cannot safely be translated into a different HTTP status here.
			slog.Error("response compression finalization failed",
				"method", r.Method, "path", r.URL.Path, "error", finishErr)
		}
	})
}

func skipCompressionRequest(r *http.Request) bool {
	if r.Method == http.MethodConnect {
		return true
	}
	// Assets already have immutable caching and may be binary. Keep this
	// middleware scoped to dynamic responses.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		return true
	}
	// Live is an SSE endpoint. Bypass it by route before wrapping the writer so
	// ResponseController/Flusher see exactly the same chain as before.
	if strings.HasSuffix(r.URL.Path, "/_stream") {
		return true
	}
	if r.Header.Get("Upgrade") != "" || headerHasToken(r.Header.Values("Connection"), "upgrade") {
		return true
	}
	return false
}

// headResponseWriter preserves response headers, status and controller access
// while discarding the selected representation's bytes. compressionWriter
// therefore makes exactly the same gzip/identity decision as GET without
// relying on net/http's later HEAD-body suppression.
type headResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteFinal  bool
	committed   bool
	flushed     bool
	selectedLen int
}

func (w *headResponseWriter) WriteHeader(status int) {
	if w.wroteFinal {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.wroteFinal = true
}

func (w *headResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteFinal {
		w.status = http.StatusOK
		w.wroteFinal = true
	}
	w.selectedLen += len(p)
	return len(p), nil
}

func (w *headResponseWriter) FlushError() error {
	w.flushed = true
	w.commit()
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *headResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *headResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *headResponseWriter) finish() {
	w.commit()
}

func (w *headResponseWriter) commit() {
	if w.committed {
		return
	}
	if !w.wroteFinal {
		w.status = http.StatusOK
		w.wroteFinal = true
	}
	// RFC 9110 permits HEAD to report the exact selected-representation length
	// even when GET's protocol framing chooses chunking and omits the field.
	if !w.flushed && w.selectedLen > 0 && statusAllowsBody(w.status) &&
		w.Header().Get("Content-Length") == "" && w.Header().Get("Transfer-Encoding") == "" && !hasResponseTrailers(w.Header()) {
		w.Header().Set("Content-Length", strconv.Itoa(w.selectedLen))
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(w.status)
}

// compressionWriter delays a normal response only until minCompressSize bytes
// are available. It then has enough data to sniff a missing Content-Type and
// can safely choose gzip or identity. Explicit streaming content types and
// Flush calls commit identity immediately.
type compressionWriter struct {
	http.ResponseWriter

	acceptsGzip        bool
	requestNoTransform bool
	status             int
	wroteHeader        bool
	committed          bool
	compressing        bool
	buffer             []byte
	gzipWriter         *gzip.Writer
	writeErr           error
}

func (w *compressionWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// Informational responses are not the final response and may be followed
		// by a regular compressible body (for example, 103 Early Hints).
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.wroteHeader = true
	w.status = status
	if !statusAllowsBody(status) {
		w.commitIdentity()
	}
}

func (w *compressionWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	if !statusAllowsBody(w.status) {
		// A handler can mistakenly write after 1xx/204/205/304. Do not pass
		// those bytes to permissive ResponseWriter implementations such as
		// ResponseRecorder, and do not rely on net/http to reject them.
		return 0, http.ErrBodyNotAllowed
	}
	if w.committed {
		var (
			n   int
			err error
		)
		if w.compressing {
			n, err = w.gzipWriter.Write(p)
		} else {
			n, err = w.ResponseWriter.Write(p)
		}
		if err == nil && n < len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.writeErr = err
		}
		return n, err
	}

	originalLen := len(p)
	previousBuffered := len(w.buffer)
	buffered := w.bufferPrefix(p)
	if len(w.buffer) < minCompressSize {
		return originalLen, nil
	}
	acceptedBuffer, err := w.commitBuffered()
	acceptedCurrent := acceptedBuffer - previousBuffered
	if acceptedCurrent < 0 {
		acceptedCurrent = 0
	} else if acceptedCurrent > buffered {
		acceptedCurrent = buffered
	}
	if err != nil {
		w.writeErr = err
		return acceptedCurrent, err
	}
	remainder := p[buffered:]
	if len(remainder) == 0 {
		return originalLen, nil
	}
	var n int
	if w.compressing {
		n, err = w.gzipWriter.Write(remainder)
	} else {
		n, err = w.ResponseWriter.Write(remainder)
	}
	if err == nil && n < len(remainder) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.writeErr = err
	}
	return acceptedCurrent + n, err
}

// bufferPrefix retains only enough of p to reach the sniff/decision threshold.
// Allocating the slice at its final capacity keeps both len and cap bounded by
// minCompressSize even when the handler makes one multi-megabyte Write.
func (w *compressionWriter) bufferPrefix(p []byte) int {
	if len(p) == 0 || len(w.buffer) >= minCompressSize {
		return 0
	}
	if w.buffer == nil {
		w.buffer = make([]byte, 0, minCompressSize)
	}
	n := min(minCompressSize-len(w.buffer), len(p))
	w.buffer = append(w.buffer, p[:n]...)
	return n
}

// FlushError makes http.NewResponseController(w).Flush work through this
// wrapper. A flush before the gzip decision marks the response as streaming
// and commits it unchanged; if gzip has already begun, both gzip and the
// underlying connection are flushed in order.
func (w *compressionWriter) FlushError() error {
	if !w.committed {
		if !w.wroteHeader {
			// Flush commits an implicit 200 in net/http. Record that same final
			// state here before forwarding it so a later WriteHeader cannot make
			// the wrapper believe the wire became a bodyless response.
			w.wroteHeader = true
			w.status = http.StatusOK
		}
		w.prepareRepresentationMetadata(w.bufferedContentType())
		w.commitIdentity()
		if len(w.buffer) > 0 {
			if _, err := w.ResponseWriter.Write(w.buffer); err != nil {
				w.buffer = w.buffer[:0]
				w.writeErr = err
				return err
			}
			w.buffer = w.buffer[:0]
		}
	}
	if w.compressing {
		if err := w.gzipWriter.Flush(); err != nil {
			w.writeErr = err
			return err
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *compressionWriter) Flush() {
	_ = w.FlushError()
}

// Unwrap keeps every ResponseController operation other than Flush (deadlines,
// full-duplex and hijacking) reachable through the standard wrapper chain.
func (w *compressionWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *compressionWriter) finish() error {
	if !w.committed {
		if len(w.buffer) >= minCompressSize {
			if _, err := w.commitBuffered(); err != nil {
				w.writeErr = err
			}
		} else {
			w.prepareRepresentationMetadata(w.bufferedContentType())
			w.commitIdentity()
			if len(w.buffer) > 0 {
				_, w.writeErr = w.ResponseWriter.Write(w.buffer)
				w.buffer = w.buffer[:0]
			}
		}
	}
	if w.gzipWriter != nil {
		if err := w.gzipWriter.Close(); w.writeErr == nil {
			w.writeErr = err
		}
		w.gzipWriter.Reset(io.Discard)
		gzipWriters.Put(w.gzipWriter)
		w.gzipWriter = nil
	}
	return w.writeErr
}

func (w *compressionWriter) commitBuffered() (int, error) {
	contentType := w.bufferedContentType()
	canVary := w.prepareRepresentationMetadata(contentType)
	eligible := canVary && !w.requestNoTransform && statusAllowsBody(w.status)
	if !eligible || !w.acceptsGzip {
		w.commitIdentity()
		if len(w.buffer) == 0 {
			return 0, nil
		}
		bufferLen := len(w.buffer)
		n, err := w.ResponseWriter.Write(w.buffer)
		w.buffer = w.buffer[:0]
		if err == nil && n < bufferLen {
			err = io.ErrShortWrite
		}
		return n, err
	}

	w.Header().Set("Content-Encoding", "gzip")
	// A handler-supplied length describes the identity representation. The
	// compressed length is not known while streaming, so let net/http frame it.
	w.Header().Del("Content-Length")
	w.committed = true
	w.compressing = true
	w.writeFinalHeader()
	w.gzipWriter = gzipWriters.Get().(*gzip.Writer)
	w.gzipWriter.Reset(w.ResponseWriter)
	bufferLen := len(w.buffer)
	n, err := w.gzipWriter.Write(w.buffer)
	w.buffer = w.buffer[:0]
	if err == nil && n < bufferLen {
		err = io.ErrShortWrite
	}
	return n, err
}

func hasResponseTrailers(header http.Header) bool {
	if values, ok := header["Trailer"]; ok && len(values) > 0 {
		return true
	}
	for key := range header {
		if strings.HasPrefix(key, http.TrailerPrefix) {
			return true
		}
	}
	return false
}

func (w *compressionWriter) bufferedContentType() string {
	contentType := w.Header().Get("Content-Type")
	if contentType == "" && len(w.buffer) > 0 {
		contentType = http.DetectContentType(w.buffer)
		// Without this, the underlying server would sniff gzip framing, while
		// a HEAD discard would have no bytes from which to infer GET metadata.
		w.Header().Set("Content-Type", contentType)
	}
	return contentType
}

func (w *compressionWriter) commitIdentity() {
	if w.committed {
		return
	}
	w.committed = true
	w.writeFinalHeader()
}

func (w *compressionWriter) writeFinalHeader() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status == http.StatusNotModified {
		// A 304 carries metadata for the selected representation. When that
		// representation is textually compressible, caches need the same
		// negotiation contract as the corresponding 200 response.
		w.prepareRepresentationMetadata(w.Header().Get("Content-Type"))
	}
	if statusDisallowsContentLength(w.status) {
		w.Header().Del("Content-Length")
	}
	if w.wroteHeader || w.status != http.StatusOK {
		w.ResponseWriter.WriteHeader(w.status)
	}
}

// prepareRepresentationMetadata applies the cache contract shared by identity,
// gzip and bodyless 304 responses. Both Accept-Encoding and a request
// Cache-Control: no-transform directive can select a different representation,
// so every transformable variant must advertise both request fields.
func (w *compressionWriter) prepareRepresentationMetadata(contentType string) bool {
	if !w.representationCanVary(contentType) {
		return false
	}
	addVary(w.Header(), "Accept-Encoding")
	addVary(w.Header(), "Cache-Control")
	weakenStrongETag(w.Header())
	return true
}

func (w *compressionWriter) representationCanVary(contentType string) bool {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if (!statusAllowsBody(status) && status != http.StatusNotModified) || status == http.StatusPartialContent {
		return false
	}
	if hasDirective(w.Header().Values("Cache-Control"), "no-transform") {
		return false
	}
	if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Content-Range") != "" {
		return false
	}
	if status == http.StatusNotModified && strings.TrimSpace(contentType) == "" {
		// Content-Type is commonly omitted from 304. Conservatively retain the
		// negotiation/validator contract unless metadata proves the response
		// cannot be a transformable representation.
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType == "text/event-stream" || strings.HasPrefix(mediaType, "multipart/") {
		return false
	}
	return compressibleMediaType(mediaType)
}

func statusAllowsBody(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusResetContent && status != http.StatusNotModified
}

func statusDisallowsContentLength(status int) bool {
	// A handler-supplied 304 length describes its identity representation. This
	// middleware cannot derive the selected gzip length from a bodyless response,
	// so omitting it is the only validator-safe choice.
	return status >= 100 && status < 200 || status == http.StatusNoContent || status == http.StatusResetContent || status == http.StatusNotModified
}

func compressibleMediaType(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	if strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") {
		return true
	}
	switch mediaType {
	case "application/json",
		"application/javascript",
		"application/x-javascript",
		"application/ecmascript",
		"application/xml",
		"application/xhtml+xml",
		"application/yaml",
		"application/x-yaml",
		"application/vnd.yaml",
		"application/graphql",
		"application/sql",
		"application/toml",
		"image/svg+xml":
		return true
	default:
		return false
	}
}

// acceptsEncoding applies RFC 9110 qvalue syntax for one server-supported
// coding. An explicit coding always overrides '*'; q=0 and a malformed weight
// reject it. Duplicate valid entries use their highest quality, while a
// malformed explicit occurrence fails closed instead of falling back to '*'.
func acceptsEncoding(values []string, coding string) bool {
	wanted := strings.ToLower(coding)
	var (
		explicit        bool
		explicitInvalid bool
		explicitQ       int
		wildcard        bool
		wildcardInvalid bool
		wildcardQ       int
	)
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			parts := strings.Split(item, ";")
			name := strings.ToLower(strings.TrimSpace(parts[0]))
			if name == "" {
				continue
			}
			quality, valid := 1000, isToken(name) || name == "*"
			if len(parts) > 1 {
				valid = valid && len(parts) == 2
				parameter := strings.TrimSpace(parts[1])
				if len(parameter) < 3 || !strings.EqualFold(parameter[:2], "q=") {
					valid = false
				} else if parsed, ok := parseQvalue(parameter[2:]); ok {
					quality = parsed
				} else {
					valid = false
				}
			}
			switch name {
			case wanted:
				if !valid {
					explicitInvalid = true
				}
				if !explicit || quality > explicitQ {
					explicitQ = quality
				}
				explicit = true
			case "*":
				if !valid {
					wildcardInvalid = true
				}
				if !wildcard || quality > wildcardQ {
					wildcardQ = quality
				}
				wildcard = true
			}
		}
	}
	if explicit {
		return !explicitInvalid && explicitQ > 0
	}
	return wildcard && !wildcardInvalid && wildcardQ > 0
}

// parseQvalue returns thousandths so comparisons need no floating point. RFC
// 9110 permits exactly 0[.0-3DIGIT] or 1[.0-3("0")], including a bare dot.
func parseQvalue(value string) (int, bool) {
	if value == "0" {
		return 0, true
	}
	if value == "1" {
		return 1000, true
	}
	if len(value) < 2 || len(value) > 5 || value[1] != '.' {
		return 0, false
	}
	digits := value[2:]
	switch value[0] {
	case '0':
		quality := 0
		place := 100
		for i := 0; i < len(digits); i++ {
			if digits[i] < '0' || digits[i] > '9' {
				return 0, false
			}
			quality += int(digits[i]-'0') * place
			place /= 10
		}
		return quality, true
	case '1':
		for i := 0; i < len(digits); i++ {
			if digits[i] != '0' {
				return 0, false
			}
		}
		return 1000, true
	default:
		return 0, false
	}
}

func isToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
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

func hasDirective(values []string, directive string) bool {
	return headerHasToken(values, directive)
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
