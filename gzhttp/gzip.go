package gzhttp

import (
	"bufio"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzhttp/writer"
	"github.com/klauspost/compress/gzhttp/writer/gzkp"
	"github.com/klauspost/compress/gzip"
)

const (
	// HeaderNoCompression can be used to disable compression.
	// Any header value will disable compression.
	// The Header is always removed from output.
	HeaderNoCompression = "No-Gzip-Compression"

	vary            = "Vary"
	acceptEncoding  = "Accept-Encoding"
	contentEncoding = "Content-Encoding"
	contentRange    = "Content-Range"
	acceptRanges    = "Accept-Ranges"
	contentType     = "Content-Type"
	contentLength   = "Content-Length"
)

type codings map[string]float64

const (
	// DefaultQValue is the default qvalue to assign to an encoding if no explicit qvalue is set.
	// This is actually kind of ambiguous in RFC 2616, so hopefully it's correct.
	// The examples seem to indicate that it is.
	DefaultQValue = 1.0

	// DefaultMinSize is the default minimum size until we enable gzip compression.
	// 1500 bytes is the MTU size for the internet since that is the largest size allowed at the network layer.
	// If you take a file that is 1300 bytes and compress it to 800 bytes, it’s still transmitted in that same 1500 byte packet regardless, so you’ve gained nothing.
	// That being the case, you should restrict the gzip compression to files with a size (plus header) greater than a single packet,
	// 1024 bytes (1KB) is therefore default.
	DefaultMinSize = 1024
)

// GzipResponseWriter provides an http.ResponseWriter interface, which gzips
// bytes before writing them to the underlying response. This doesn't close the
// writers, so don't forget to do that.
// It can be configured to skip response smaller than minSize.
type GzipResponseWriter struct {
	http.ResponseWriter
	level     int
	gwFactory writer.GzipWriterFactory
	gw        writer.GzipWriter

	code int // Saves the WriteHeader value.

	minSize          int    // Specifies the minimum response size to gzip. If the response length is bigger than this value, it is compressed.
	buf              []byte // Holds the first part of the write before reaching the minSize or the end of the write.
	ignore           bool   // If true, then we immediately passthru writes to the underlying ResponseWriter.
	keepAcceptRanges bool   // Keep "Accept-Ranges" header.

	contentTypeFilter func(ct string) bool // Only compress if the response is one of these content-types. All are accepted if empty.
}

type GzipResponseWriterWithCloseNotify struct {
	*GzipResponseWriter
}

func (w GzipResponseWriterWithCloseNotify) CloseNotify() <-chan bool {
	return w.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

// Write appends data to the gzip writer.
func (w *GzipResponseWriter) Write(b []byte) (int, error) {
	// GZIP responseWriter is initialized. Use the GZIP responseWriter.
	if w.gw != nil {
		return w.gw.Write(b)
	}

	// If we have already decided not to use GZIP, immediately passthrough.
	if w.ignore {
		return w.ResponseWriter.Write(b)
	}

	// Save the write into a buffer for later use in GZIP responseWriter
	// (if content is long enough) or at close with regular responseWriter.
	wantBuf := 512
	if w.minSize > wantBuf {
		wantBuf = w.minSize
	}
	toAdd := len(b)
	if len(w.buf)+toAdd > wantBuf {
		toAdd = wantBuf - len(w.buf)
	}
	w.buf = append(w.buf, b[:toAdd]...)
	remain := b[toAdd:]

	// Only continue if they didn't already choose an encoding or a known unhandled content length or type.
	if len(w.Header()[HeaderNoCompression]) == 0 && w.Header().Get(contentEncoding) == "" && w.Header().Get(contentRange) == "" {
		// Check more expensive parts now.
		cl, _ := atoi(w.Header().Get(contentLength))
		ct := w.Header().Get(contentType)
		if cl == 0 || cl >= w.minSize && (ct == "" || w.contentTypeFilter(ct)) {
			// If the current buffer is less than minSize and a Content-Length isn't set, then wait until we have more data.
			if len(w.buf) < w.minSize && cl == 0 {
				return len(b), nil
			}

			// If the Content-Length is larger than minSize or the current buffer is larger than minSize, then continue.
			if cl >= w.minSize || len(w.buf) >= w.minSize {
				// If a Content-Type wasn't specified, infer it from the current buffer.
				if ct == "" {
					ct = http.DetectContentType(w.buf)
				}

				// Handles the intended case of setting a nil Content-Type (as for http/server or http/fs)
				// Set the header only if the key does not exist
				if _, ok := w.Header()[contentType]; !ok {
					w.Header().Set(contentType, ct)
				}

				// If the Content-Type is acceptable to GZIP, initialize the GZIP writer.
				if w.contentTypeFilter(ct) {
					if err := w.startGzip(); err != nil {
						return 0, err
					}
					if len(remain) > 0 {
						if _, err := w.gw.Write(remain); err != nil {
							return 0, err
						}
					}
					return len(b), nil
				}
			}
		}
	}
	// If we got here, we should not GZIP this response.
	if err := w.startPlain(); err != nil {
		return 0, err
	}
	if len(remain) > 0 {
		if _, err := w.ResponseWriter.Write(remain); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// startGzip initializes a GZIP writer and writes the buffer.
func (w *GzipResponseWriter) startGzip() error {
	// Set the GZIP header.
	w.Header().Set(contentEncoding, "gzip")

	// if the Content-Length is already set, then calls to Write on gzip
	// will fail to set the Content-Length header since its already set
	// See: https://github.com/golang/go/issues/14975.
	w.Header().Del(contentLength)

	// Delete Accept-Ranges.
	if !w.keepAcceptRanges {
		w.Header().Del(acceptRanges)
	}

	// Write the header to gzip response.
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}

	// Initialize and flush the buffer into the gzip response if there are any bytes.
	// If there aren't any, we shouldn't initialize it yet because on Close it will
	// write the gzip header even if nothing was ever written.
	if len(w.buf) > 0 {
		// Initialize the GZIP response.
		w.init()
		n, err := w.gw.Write(w.buf)

		// This should never happen (per io.Writer docs), but if the write didn't
		// accept the entire buffer but returned no specific error, we have no clue
		// what's going on, so abort just to be safe.
		if err == nil && n < len(w.buf) {
			err = io.ErrShortWrite
		}
		w.buf = w.buf[:0]
		return err
	}
	return nil
}

// startPlain writes to sent bytes and buffer the underlying ResponseWriter without gzip.
func (w *GzipResponseWriter) startPlain() error {
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}
	delete(w.Header(), HeaderNoCompression)
	w.ignore = true
	// If Write was never called then don't call Write on the underlying ResponseWriter.
	if len(w.buf) == 0 {
		return nil
	}
	n, err := w.ResponseWriter.Write(w.buf)
	// This should never happen (per io.Writer docs), but if the write didn't
	// accept the entire buffer but returned no specific error, we have no clue
	// what's going on, so abort just to be safe.
	if err == nil && n < len(w.buf) {
		err = io.ErrShortWrite
	}

	w.buf = w.buf[:0]
	return err
}

// WriteHeader just saves the response code until close or GZIP effective writes.
func (w *GzipResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

// init graps a new gzip writer from the gzipWriterPool and writes the correct
// content encoding header.
func (w *GzipResponseWriter) init() {
	// Bytes written during ServeHTTP are redirected to this gzip writer
	// before being written to the underlying response.
	w.gw = w.gwFactory.New(w.ResponseWriter, w.level)
}

// Close will close the gzip.Writer and will put it back in the gzipWriterPool.
func (w *GzipResponseWriter) Close() error {
	if w.ignore {
		return nil
	}

	if w.gw == nil {
		// GZIP not triggered yet, write out regular response.
		err := w.startPlain()
		// Returns the error if any at write.
		if err != nil {
			err = fmt.Errorf("gziphandler: write to regular responseWriter at close gets error: %q", err.Error())
		}
		return err
	}

	err := w.gw.Close()
	w.gw = nil
	return err
}

// Flush flushes the underlying *gzip.Writer and then the underlying
// http.ResponseWriter if it is an http.Flusher. This makes GzipResponseWriter
// an http.Flusher.
// If not enough bytes has been written to determine if we have reached minimum size,
// this will be ignored.
// If nothing has been written yet, nothing will be flushed.
func (w *GzipResponseWriter) Flush() {
	if w.gw == nil && !w.ignore {
		if len(w.buf) == 0 {
			// Nothing written yet.
			return
		}
		var (
			cl, _ = atoi(w.Header().Get(contentLength))
			ct    = w.Header().Get(contentType)
			ce    = w.Header().Get(contentEncoding)
			cr    = w.Header().Get(contentRange)
		)

		if ct == "" {
			ct = http.DetectContentType(w.buf)

			// Handles the intended case of setting a nil Content-Type (as for http/server or http/fs)
			// Set the header only if the key does not exist
			if _, ok := w.Header()[contentType]; !ok {
				w.Header().Set(contentType, ct)
			}
		}
		if cl == 0 {
			// Assume minSize.
			cl = w.minSize
		}

		// See if we should compress...
		if len(w.Header()[HeaderNoCompression]) == 0 && ce == "" && cr == "" && cl >= w.minSize && w.contentTypeFilter(ct) {
			w.startGzip()
		} else {
			w.startPlain()
		}
	}

	if w.gw != nil {
		w.gw.Flush()
	}

	if fw, ok := w.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

// Hijack implements http.Hijacker. If the underlying ResponseWriter is a
// Hijacker, its Hijack method is returned. Otherwise an error is returned.
func (w *GzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}

// verify Hijacker interface implementation
var _ http.Hijacker = &GzipResponseWriter{}

var onceDefault sync.Once
var defaultWrapper func(http.Handler) http.Handler

// GzipHandler allows to easily wrap an http handler with default settings.
func GzipHandler(h http.Handler) http.Handler {
	onceDefault.Do(func() {
		var err error
		defaultWrapper, err = NewWrapper()
		if err != nil {
			panic(err)
		}
	})

	return defaultWrapper(h)
}

var grwPool = sync.Pool{New: func() interface{} { return &GzipResponseWriter{} }}

// NewWrapper returns a reusable wrapper with the supplied options.
func NewWrapper(opts ...option) (func(http.Handler) http.Handler, error) {
	c := &config{
		level:   gzip.DefaultCompression,
		minSize: DefaultMinSize,
		writer: writer.GzipWriterFactory{
			Levels: gzkp.Levels,
			New:    gzkp.NewWriter,
		},
		contentTypes: DefaultContentTypeFilter,
	}

	for _, o := range opts {
		o(c)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add(vary, acceptEncoding)
			if acceptsGzip(r) {
				gw := grwPool.Get().(*GzipResponseWriter)
				*gw = GzipResponseWriter{
					ResponseWriter:    w,
					gwFactory:         c.writer,
					level:             c.level,
					minSize:           c.minSize,
					contentTypeFilter: c.contentTypes,
					keepAcceptRanges:  c.keepAcceptRanges,
					buf:               gw.buf,
				}
				if len(gw.buf) > 0 {
					gw.buf = gw.buf[:0]
				}
				defer func() {
					gw.Close()
					gw.ResponseWriter = nil
					grwPool.Put(gw)
				}()

				if _, ok := w.(http.CloseNotifier); ok {
					gwcn := GzipResponseWriterWithCloseNotify{gw}
					h.ServeHTTP(gwcn, r)
				} else {
					h.ServeHTTP(gw, r)
				}

			} else {
				h.ServeHTTP(w, r)
			}
		})
	}, nil
}

// Parsed representation of one of the inputs to ContentTypes.
// See https://golang.org/pkg/mime/#ParseMediaType
type parsedContentType struct {
	mediaType string
	params    map[string]string
}

// equals returns whether this content type matches another content type.
func (pct parsedContentType) equals(mediaType string, params map[string]string) bool {
	if pct.mediaType != mediaType {
		return false
	}
	// if pct has no params, don't care about other's params
	if len(pct.params) == 0 {
		return true
	}

	// if pct has any params, they must be identical to other's.
	if len(pct.params) != len(params) {
		return false
	}
	for k, v := range pct.params {
		if w, ok := params[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// Used for functional configuration.
type config struct {
	minSize          int
	level            int
	writer           writer.GzipWriterFactory
	contentTypes     func(ct string) bool
	keepAcceptRanges bool
}

func (c *config) validate() error {
	min, max := c.writer.Levels()
	if c.level < min || c.level > max {
		return fmt.Errorf("invalid compression level requested: %d, valid range %d -> %d", c.level, min, max)
	}

	if c.minSize < 0 {
		return fmt.Errorf("minimum size must be more than zero")
	}

	return nil
}

type option func(c *config)

func MinSize(size int) option {
	return func(c *config) {
		c.minSize = size
	}
}

// CompressionLevel sets the compression level
func CompressionLevel(level int) option {
	return func(c *config) {
		c.level = level
	}
}

// Implementation changes the implementation of GzipWriter
//
// The default implementation is writer/stdlib/NewWriter
// which is backed by standard library's compress/zlib
func Implementation(writer writer.GzipWriterFactory) option {
	return func(c *config) {
		c.writer = writer
	}
}

// ContentTypes specifies a list of content types to compare
// the Content-Type header to before compressing. If none
// match, the response will be returned as-is.
//
// Content types are compared in a case-insensitive, whitespace-ignored
// manner.
//
// A MIME type without any other directive will match a content type
// that has the same MIME type, regardless of that content type's other
// directives. I.e., "text/html" will match both "text/html" and
// "text/html; charset=utf-8".
//
// A MIME type with any other directive will only match a content type
// that has the same MIME type and other directives. I.e.,
// "text/html; charset=utf-8" will only match "text/html; charset=utf-8".
//
// By default common compressed audio, video and archive formats, see DefaultContentTypeFilter.
//
// Setting this will override default and any previous Content Type settings.
func ContentTypes(types []string) option {
	return func(c *config) {
		var contentTypes []parsedContentType
		for _, v := range types {
			mediaType, params, err := mime.ParseMediaType(v)
			if err == nil {
				contentTypes = append(contentTypes, parsedContentType{mediaType, params})
			}
		}
		c.contentTypes = func(ct string) bool {
			return handleContentType(contentTypes, ct)
		}
	}
}

// ExceptContentTypes specifies a list of content types to compare
// the Content-Type header to before compressing. If none
// match, the response will be compressed.
//
// Content types are compared in a case-insensitive, whitespace-ignored
// manner.
//
// A MIME type without any other directive will match a content type
// that has the same MIME type, regardless of that content type's other
// directives. I.e., "text/html" will match both "text/html" and
// "text/html; charset=utf-8".
//
// A MIME type with any other directive will only match a content type
// that has the same MIME type and other directives. I.e.,
// "text/html; charset=utf-8" will only match "text/html; charset=utf-8".
//
// By default common compressed audio, video and archive formats, see DefaultContentTypeFilter.
//
// Setting this will override default and any previous Content Type settings.
func ExceptContentTypes(types []string) option {
	return func(c *config) {
		var contentTypes []parsedContentType
		for _, v := range types {
			mediaType, params, err := mime.ParseMediaType(v)
			if err == nil {
				contentTypes = append(contentTypes, parsedContentType{mediaType, params})
			}
		}
		c.contentTypes = func(ct string) bool {
			return !handleContentType(contentTypes, ct)
		}
	}
}

// KeepAcceptRanges will keep Accept-Ranges header on gzipped responses.
// This will likely break ranged requests since that cannot be transparently
// handled by the filter.
func KeepAcceptRanges() option {
	return func(c *config) {
		c.keepAcceptRanges = true
	}
}

// ContentTypeFilter allows adding a custom content type filter.
//
// The supplied function must return true/false to indicate if content
// should be compressed.
//
// When called no parsing of the content type 'ct' has been done.
// It may have been set or auto-detected.
//
// Setting this will override default and any previous Content Type settings.
func ContentTypeFilter(compress func(ct string) bool) option {
	return func(c *config) {
		c.contentTypes = compress
	}
}

// acceptsGzip returns true if the given HTTP request indicates that it will
// accept a gzipped response.
func acceptsGzip(r *http.Request) bool {
	// Note that we don't request this for HEAD requests,
	// due to a bug in nginx:
	//   https://trac.nginx.org/nginx/ticket/358
	//   https://golang.org/issue/5522
	return r.Method != http.MethodHead && parseEncodingGzip(r.Header.Get(acceptEncoding)) > 0
}

// returns true if we've been configured to compress the specific content type.
func handleContentType(contentTypes []parsedContentType, ct string) bool {
	// If contentTypes is empty we handle all content types.
	if len(contentTypes) == 0 {
		return true
	}

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}

	for _, c := range contentTypes {
		if c.equals(mediaType, params) {
			return true
		}
	}

	return false
}

// parseEncodingGzip returns the qvalue of gzip compression.
func parseEncodingGzip(s string) float64 {
	s = strings.TrimSpace(s)

	for len(s) > 0 {
		stop := strings.IndexByte(s, ',')
		if stop < 0 {
			stop = len(s)
		}
		coding, qvalue, _ := parseCoding(s[:stop])

		if coding == "gzip" {
			return qvalue
		}
		if stop == len(s) {
			break
		}
		s = s[stop+1:]
	}
	return 0
}

func parseEncodings(s string) (codings, error) {
	split := strings.Split(s, ",")
	c := make(codings, len(split))
	var e []string

	for _, ss := range split {
		coding, qvalue, err := parseCoding(ss)

		if err != nil {
			e = append(e, err.Error())
		} else {
			c[coding] = qvalue
		}
	}

	// TODO (adammck): Use a proper multi-error struct, so the individual errors
	//                 can be extracted if anyone cares.
	if len(e) > 0 {
		return c, fmt.Errorf("errors while parsing encodings: %s", strings.Join(e, ", "))
	}

	return c, nil
}

// parseCoding parses a single coding (content-coding with an optional qvalue),
// as might appear in an Accept-Encoding header. It attempts to forgive minor
// formatting errors.
func parseCoding(s string) (coding string, qvalue float64, err error) {
	for n, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		qvalue = DefaultQValue

		if n == 0 {
			coding = strings.ToLower(part)
		} else if strings.HasPrefix(part, "q=") {
			qvalue, err = strconv.ParseFloat(strings.TrimPrefix(part, "q="), 64)

			if qvalue < 0.0 {
				qvalue = 0.0
			} else if qvalue > 1.0 {
				qvalue = 1.0
			}
		}
	}

	if coding == "" {
		err = fmt.Errorf("empty content-coding")
	}

	return
}

// Don't compress any audio/video types.
var excludePrefixDefault = []string{"video/", "audio/"}

// Skip a bunch of compressed types that contains this string.
var excludeContainsDefault = []string{"compress", "zip"}

// DefaultContentTypeFilter excludes common compressed audio, video and archive formats.
func DefaultContentTypeFilter(ct string) bool {
	ct = strings.TrimSpace(strings.ToLower(ct))
	if ct == "" {
		return true
	}
	for _, s := range excludeContainsDefault {
		if strings.Contains(ct, s) {
			return false
		}
	}

	for _, prefix := range excludePrefixDefault {
		if strings.HasPrefix(ct, prefix) {
			return false
		}
	}
	return true
}

// CompressAllContentTypeFilter will compress all mime types.
func CompressAllContentTypeFilter(ct string) bool {
	return true
}

const intSize = 32 << (^uint(0) >> 63)

// atoi is equivalent to ParseInt(s, 10, 0), converted to type int.
func atoi(s string) (int, bool) {
	sLen := len(s)
	if intSize == 32 && (0 < sLen && sLen < 10) ||
		intSize == 64 && (0 < sLen && sLen < 19) {
		// Fast path for small integers that fit int type.
		s0 := s
		if s[0] == '-' || s[0] == '+' {
			s = s[1:]
			if len(s) < 1 {
				return 0, false
			}
		}

		n := 0
		for _, ch := range []byte(s) {
			ch -= '0'
			if ch > 9 {
				return 0, false
			}
			n = n*10 + int(ch)
		}
		if s0[0] == '-' {
			n = -n
		}
		return n, true
	}

	// Slow path for invalid, big, or underscored integers.
	i64, err := strconv.ParseInt(s, 10, 0)
	return int(i64), err == nil
}
