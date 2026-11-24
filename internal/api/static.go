package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tokemon/tokemon/internal/art"
	"github.com/tokemon/tokemon/web"
)

const staticImmutableCacheControl = "public, max-age=31536000, immutable"

var embeddedStaticVersion = sync.OnceValue(func() string {
	staticFiles, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return "dev"
	}
	version, err := computeStaticAssetVersion(staticFiles)
	if err != nil || version == "" {
		return "dev"
	}
	return version
})

func computeStaticAssetVersion(fsys fs.FS) (string, error) {
	var paths []string
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func staticAssetURL(path, version string) string {
	if version == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "v=" + url.QueryEscape(version)
}

func loadArtDimensions(fsys fs.FS) map[int][2]int {
	dims := make(map[int][2]int)
	if fsys == nil {
		return dims
	}
	data, err := fs.ReadFile(fsys, "tokemon/manifest.json")
	if err != nil {
		return dims
	}
	var manifest art.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return dims
	}
	for key, asset := range manifest.Assets {
		stage, err := strconv.Atoi(key)
		if err != nil || stage < 0 || asset.Width <= 0 || asset.Height <= 0 {
			continue
		}
		dims[stage] = [2]int{asset.Width, asset.Height}
	}
	return dims
}

func staticCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowImmutable := !strings.HasSuffix(r.URL.Path, "/")
		sw := &staticCacheResponseWriter{
			ResponseWriter: w,
			allowImmutable: allowImmutable,
		}
		next.ServeHTTP(sw, r)
	})
}

type staticCacheResponseWriter struct {
	http.ResponseWriter
	allowImmutable bool
	wroteHeader    bool
}

func (w *staticCacheResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if w.allowImmutable && (status == http.StatusOK || status == http.StatusPartialContent || status == http.StatusNotModified) {
		w.Header().Set("Cache-Control", staticImmutableCacheControl)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *staticCacheResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *staticCacheResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
