package nodejs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func fakeNode(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "#!/bin/sh\necho " + version + "\n"
	tw.WriteHeader(&tar.Header{Name: "node-" + version + "/bin/node", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestBinary(t *testing.T) {
	tgz := fakeNode(t, "v24.2.0")
	sum := sha256.Sum256(tgz)
	file := fmt.Sprintf("node-v24.2.0-linux-%s.tar.gz", arch())
	var corrupt atomic.Bool
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			fmt.Fprint(w, `[{"version":"v25.0.0"},{"version":"v24.2.0"},{"version":"v24.1.0"},{"version":"v20.19.0"}]`)
		case "/v24.2.0/SHASUMS256.txt":
			fmt.Fprintf(w, "%s  other.tar.gz\n%s  %s\n", strings.Repeat("0", 64), hex.EncodeToString(sum[:]), file)
		case "/v24.2.0/" + file:
			downloads.Add(1)
			if corrupt.Load() {
				w.Write(fakeNode(t, "v6.6.6"))
				return
			}
			w.Write(tgz)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	rt := New(dir)
	rt.BaseURL = srv.URL
	ctx := context.Background()
	bin, err := rt.Binary(ctx, "node24")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(bin); err != nil || !strings.Contains(string(b), "v24.2.0") {
		t.Errorf("binary %s: %q %v", bin, b, err)
	}
	// A new Runtime finds it installed, and so does one that is offline.
	rt2 := New(dir)
	rt2.BaseURL = srv.URL
	if _, err := rt2.Binary(ctx, "node24"); err != nil || downloads.Load() != 1 {
		t.Errorf("reinstalled: %v, %d downloads", err, downloads.Load())
	}
	offline := New(dir)
	offline.BaseURL = "http://127.0.0.1:1"
	if got, err := offline.Binary(ctx, "node24"); err != nil || got != bin {
		t.Errorf("offline: %s %v", got, err)
	}
	if _, err := offline.Binary(ctx, "node20"); err == nil {
		t.Error("offline without node20 installed should fail")
	}

	corrupt.Store(true)
	bad := New(t.TempDir())
	bad.BaseURL = srv.URL
	if _, err := bad.Binary(ctx, "node24"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("corrupt download: %v", err)
	}
}
