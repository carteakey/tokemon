package api

import (
	"compress/gzip"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

var gzipWriterPool = sync.Pool{
	New: func() any {
		return gzip.NewWriter(io.Discard)
	},
}

var compressibleMIMETypes = map[string]struct{}{
	"text/html":              {},
	"application/json":       {},
	"text/csv":               {},
	"text/plain":             {},
	"image/svg+xml":          {},
	"text/css":               {},
	"application/javascript": {},
	"text/javascript":        {},
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gzw := &gzipResponseWriter{
			ResponseWriter: w,
			acceptsGzip:    acceptsGzip(r),
			isHEAD:         r.Method == http.MethodHead,
		}
		defer func() {
			_ = gzw.Close()
		}()
		next.ServeHTTP(gzw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	acceptsGzip bool
	isHEAD      bool
	status      int
	decided     bool
	compress    bool
	sniffBuf    []byte
	gz          *gzip.Writer
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.decided || w.status != 0 {
		return
	}
	if status < http.StatusOK || status == http.StatusNoContent || status == http.StatusNotModified || status == http.StatusPartialContent {
		w.status = status
		w.decided = true
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if w.decided {
		if w.compress {
			return w.gz.Write(p)
		}
		return w.ResponseWriter.Write(p)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.Header().Get("Content-Type") == "" {
		w.sniffBuf = append(w.sniffBuf, p...)
		if len(w.sniffBuf) < 512 {
			return len(p), nil
		}
		w.Header().Set("Content-Type", http.DetectContentType(w.sniffBuf))
		buf := w.sniffBuf
		w.sniffBuf = nil
		if _, err := w.commit(buf); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return w.commit(p)
}

func (w *gzipResponseWriter) commit(initial []byte) (int, error) {
	ct := w.Header().Get("Content-Type")
	compressible := w.isEligibleForCompression(ct)
	if compressible {
		addVaryHeader(w.Header(), "Accept-Encoding")
	}
	w.compress = compressible && w.acceptsGzip && !w.isHEAD
	w.decided = true
	if w.compress {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.ResponseWriter.WriteHeader(w.status)
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w.ResponseWriter)
		w.gz = gz
		if len(initial) > 0 {
			return w.gz.Write(initial)
		}
		return 0, nil
	}
	w.ResponseWriter.WriteHeader(w.status)
	if len(initial) > 0 {
		return w.ResponseWriter.Write(initial)
	}
	return 0, nil
}

func (w *gzipResponseWriter) Close() error {
	if !w.decided {
		if len(w.sniffBuf) > 0 {
			if w.status == 0 {
				w.status = http.StatusOK
			}
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", http.DetectContentType(w.sniffBuf))
			}
			buf := w.sniffBuf
			w.sniffBuf = nil
			if _, err := w.commit(buf); err != nil {
				return err
			}
		} else {
			ct := w.Header().Get("Content-Type")
			if w.status != 0 || ct != "" {
				if w.isEligibleForCompression(ct) {
					addVaryHeader(w.Header(), "Accept-Encoding")
				}
				if w.status != 0 {
					w.ResponseWriter.WriteHeader(w.status)
				}
			}
			w.decided = true
		}
	}
	if w.gz != nil {
		err := w.gz.Close()
		w.gz.Reset(io.Discard)
		gzipWriterPool.Put(w.gz)
		w.gz = nil
		return err
	}
	return nil
}

func (w *gzipResponseWriter) Flush() {
	if !w.decided {
		if len(w.sniffBuf) > 0 {
			if w.status == 0 {
				w.status = http.StatusOK
			}
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", http.DetectContentType(w.sniffBuf))
			}
			buf := w.sniffBuf
			w.sniffBuf = nil
			_, _ = w.commit(buf)
		} else if w.status != 0 {
			_, _ = w.commit(nil)
		}
	}
	if w.compress && w.gz != nil {
		_ = w.gz.Flush()
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *gzipResponseWriter) isEligibleForCompression(ct string) bool {
	if w.status != 0 && (w.status < http.StatusOK || w.status == http.StatusNoContent || w.status == http.StatusNotModified || w.status == http.StatusPartialContent) {
		return false
	}
	return isCompressibleContentType(ct) &&
		w.Header().Get("Content-Encoding") == "" &&
		!hasNoTransform(w.Header()) &&
		w.Header().Get("Upgrade") == ""
}

func isCompressibleContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	} else {
		mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	}
	_, ok := compressibleMIMETypes[mediaType]
	return ok
}

func acceptsGzip(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, header := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(header, ",") {
			tokens := strings.Split(strings.TrimSpace(part), ";")
			if len(tokens) == 0 {
				continue
			}
			coding := strings.ToLower(strings.TrimSpace(tokens[0]))
			if coding != "gzip" && coding != "x-gzip" {
				continue
			}
			q := 1.0
			valid := true
			for _, param := range tokens[1:] {
				key, val, ok := strings.Cut(param, "=")
				if !ok {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(key), "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
					if err != nil || math.IsNaN(parsed) || parsed <= 0 || parsed > 1 {
						valid = false
						break
					}
					q = parsed
				}
			}
			if valid && q > 0 {
				return true
			}
		}
	}
	return false
}

func hasNoTransform(h http.Header) bool {
	for _, header := range h.Values("Cache-Control") {
		for _, part := range strings.Split(header, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "no-transform") {
				return true
			}
		}
	}
	return false
}

func addVaryHeader(h http.Header, token string) {
	for _, existing := range h.Values("Vary") {
		for _, part := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return
			}
		}
	}
	h.Add("Vary", token)
}
