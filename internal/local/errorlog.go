package local

import (
	"net/http"
	"strings"
)

// LogServerErrors wraps h so that every 5xx answer is reported to logf with the
// request and the reason the handler gave. The clients (the artifact and cache
// toolkits) only print "responded with 500", which says nothing about a full
// disk or a broken store.
func LogServerErrors(h http.Handler, logf func(format string, args ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status >= 500 {
			// The query string holds signatures; log the path only.
			logf("%s %s: HTTP %d: %s", r.Method, r.URL.Path, rec.status, strings.TrimSpace(rec.body))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	body   string // the start of an error answer, which is short JSON
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.status >= 500 && len(s.body) < 512 {
		s.body += string(p[:min(len(p), 512-len(s.body))])
	}
	return s.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
