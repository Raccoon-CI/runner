package local

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogServerErrors(t *testing.T) {
	var logged []string
	h := LogServerErrors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/full":
			w.WriteHeader(500)
			w.Write([]byte(`{"message":"write chunk: no space left on device"}` + "\n"))
		case "/bad":
			w.WriteHeader(400)
			w.Write([]byte("nope"))
		default:
			w.Write([]byte("fine"))
		}
	}), func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) })

	for _, p := range []string{"/ok", "/bad", "/full?sig=secret&exp=1"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("PATCH", p, nil))
	}
	if len(logged) != 1 {
		t.Fatalf("logged %q, want one line for the 500 only", logged)
	}
	if !strings.Contains(logged[0], "no space left on device") || !strings.Contains(logged[0], "/full") {
		t.Errorf("line lacks the reason or the path: %q", logged[0])
	}
	if strings.Contains(logged[0], "secret") {
		t.Errorf("a signature reached the log: %q", logged[0])
	}
}
