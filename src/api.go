package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// The public JSON API (https://p.infosecstreams.com/streamstatus/). Documents are
// built once, when their data changes, into an immutable apiDoc (body,
// gzipped body, ETag), and handlers only serve the current one: a request
// never waits for the worker.

// statusMaxAge is the Cache-Control max-age of status.json, in seconds.
const statusMaxAge = 15

const jsonContentType = "application/json; charset=utf-8"

// apiDoc is a prebuilt API response. Never modify one after creating it.
type apiDoc struct {
	body        []byte
	gz          []byte
	hash        string // content hash; the ETag is derived from it
	contentType string
	maxAge      int
}

// newDoc builds an apiDoc from a finished body.
func newDoc(body []byte, contentType string, maxAge int) (*apiDoc, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return &apiDoc{
		body:        body,
		gz:          buf.Bytes(),
		hash:        hex.EncodeToString(sum[:16]),
		contentType: contentType,
		maxAge:      maxAge,
	}, nil
}

// newJSONDoc builds an apiDoc holding v as JSON.
func newJSONDoc(v any, maxAge int) (*apiDoc, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return newDoc(append(body, '\n'), jsonContentType, maxAge)
}

// etag returns the strong ETag of the identity or gzip representation.
// They differ because the bytes differ.
func (d *apiDoc) etag(gzipped bool) string {
	if gzipped {
		return `"` + d.hash + `-gz"`
	}
	return `"` + d.hash + `"`
}

// setCORS allows any site to read the API.
func setCORS(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "ETag")
}

// serveDoc answers r with doc: GET/HEAD with conditional requests and
// gzip, OPTIONS for CORS preflight. A nil doc (not built yet) is a 503.
func serveDoc(w http.ResponseWriter, r *http.Request, doc *apiDoc) {
	h := w.Header()
	setCORS(h)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions:
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "If-None-Match")
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		h.Set("Allow", "GET, HEAD, OPTIONS")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if doc == nil {
		h.Set("Retry-After", "30")
		writeAPIError(w, http.StatusServiceUnavailable, "starting up, try again shortly")
		return
	}

	gzipped := gzipAccepted(r.Header.Get("Accept-Encoding"))
	tag := doc.etag(gzipped)
	h.Set("Vary", "Accept-Encoding")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(doc.maxAge))
	h.Set("ETag", tag)
	if ifNoneMatchHits(r.Header.Get("If-None-Match"), doc) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := doc.body
	if gzipped {
		body = doc.gz
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Type", doc.contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// ifNoneMatchHits reports whether an If-None-Match header names either
// representation of doc (or is "*"). Comparison is weak, as RFC 9110
// requires for If-None-Match, so a proxy that weakened our tag still gets
// a 304.
func ifNoneMatchHits(header string, doc *apiDoc) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == doc.etag(false) || t == doc.etag(true) {
			return true
		}
	}
	return false
}

// gzipAccepted reports whether an Accept-Encoding header allows gzip.
func gzipAccepted(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		coding := strings.ToLower(strings.TrimSpace(fields[0]))
		if coding != "gzip" && coding != "x-gzip" && coding != "*" {
			continue
		}
		q := 1.0
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if v, ok := strings.CutPrefix(strings.ToLower(p), "q="); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					q = f
				}
			}
		}
		if q > 0 {
			return true
		}
		if coding != "*" {
			return false
		}
	}
	return false
}

// writeAPIError writes {"error": msg} with status code, uncached.
func writeAPIError(w http.ResponseWriter, code int, msg string) {
	h := w.Header()
	setCORS(h)
	h.Set("Content-Type", jsonContentType)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	body, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(append(body, '\n'))
}

// registerAPI adds the /streamstatus/ routes to mux.
func (s *StreamersRepo) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("/streamstatus/status.json", s.handleStatus)
	mux.HandleFunc("/streamstatus/stats.json", s.handleStats)
	mux.HandleFunc("/streamstatus/stats/", s.handleStreamerStats)
	mux.HandleFunc("/streamstatus/raids.json", s.handleRaids)
	mux.HandleFunc("/streamstatus/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			serveDoc(w, r, nil)
			return
		}
		writeAPIError(w, http.StatusNotFound, "not found")
	})
}

// handleStatus serves GET /streamstatus/status.json.
func (s *StreamersRepo) handleStatus(w http.ResponseWriter, r *http.Request) {
	var doc *apiDoc
	if s.live != nil {
		doc = s.live.snapshot()
	}
	serveDoc(w, r, doc)
}
