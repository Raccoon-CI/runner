package runnercmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Raccoon-CI/runner/internal/local"
	"github.com/Raccoon-CI/lib/pkg/artifact"
	"github.com/Raccoon-CI/lib/pkg/blob"
	"github.com/Raccoon-CI/lib/pkg/cache"
)

// Services are the runtime services steps call (artifacts and the cache).
type Services struct {
	Handler http.Handler
	Base    string            // the loopback address, http://127.0.0.1:port
	Env     map[string]string // points steps on the host at base
	// jobEnv is env with a token of its own for the job id given, for a run
	// that sends jobs to machines: a leaked token then opens one job's
	// uploads, not the whole run's.
	JobEnv func(job string) (map[string]string, error)
	Stop   func()
}

// PublicServe makes LocalServices serve for machines elsewhere: on a chosen
// listen address, with every link and environment value built on the https
// address those machines reach it at (publicURL, for example a published
// port), and more handlers mounted next to the services (the dispatch
// controller's). Every route is guarded by a token or signed URL; none is open.
type PublicServe struct {
	Listen, PublicURL string
	Mount             map[string]http.Handler
}

// LocalServices serves the runtime services on one loopback port. Artifacts
// live under artifactDir, for this run; cache entries under cacheDir, which
// outlives it.
//
// A cache that cannot start is reported and left out: jobs then run without
// one, as actions/cache does when the service is missing.
func LocalServices(artifactDir, cacheDir, runID string, cacheV2 bool, cacheLimit int64, serve *PublicServe, cacheBucket blob.Store) (*Services, error) {
	listen := "127.0.0.1:0"
	if serve != nil {
		listen = serve.Listen
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		ln.Close()
		return nil, err
	}
	base := "http://" + ln.Addr().String()
	if serve != nil {
		base = serve.PublicURL // machines reach the services there, not at the listener's address
	}
	signer, err := artifact.NewSigner(key)
	if err != nil {
		ln.Close()
		return nil, err
	}
	// One token for the whole run: every job of a local run is the same user.
	scope := artifact.Scope{Run: runID, Job: "local"}
	const tokenTTL = 24 * time.Hour

	var closers []func()
	closeAll := func() {
		ln.Close()
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	mux := http.NewServeMux()

	artStore, err := blob.NewDir(artifactDir, base, key)
	if err != nil {
		closeAll()
		return nil, err
	}
	closers = append(closers, func() { artStore.Close() })
	artSvc, err := artifact.New(artifact.Config{Store: artStore, Signer: signer, BaseURL: base,
		SpoolDir: filepath.Join(artifactDir, "spool")})
	if err != nil {
		closeAll()
		return nil, err
	}
	closers = append(closers, artSvc.Close)
	env, err := artSvc.Env(scope, tokenTTL)
	if err != nil {
		closeAll()
		return nil, err
	}
	mux.Handle(blob.DownloadPrefix, artStore.Handler())

	if cacheEnv, err := startCache(mux, &closers, base, cacheDir, key, signer, scope, tokenTTL, cacheV2, cacheLimit, cacheBucket); err != nil {
		fmt.Fprintf(os.Stderr, "raccoonci: cache disabled: %v\n", err)
	} else {
		for k, v := range cacheEnv {
			env[k] = v
		}
	}

	if serve != nil {
		for prefix, h := range serve.Mount {
			mux.Handle(prefix, h)
		}
	}
	// Only the artifact routes sit at "/"; the cache has its own prefix.
	mux.Handle("/", artSvc.Handler())
	handler := local.LogServerErrors(mux, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "raccoonci: artifact/cache service: "+format+"\n", a...)
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	jobEnv := func(job string) (map[string]string, error) {
		tok, err := signer.Token(artifact.Scope{Run: runID, Job: job}, tokenTTL)
		if err != nil {
			return nil, err
		}
		e := maps.Clone(env)
		e["ACTIONS_RUNTIME_TOKEN"] = tok // the artifact and cache services share one token format
		return e, nil
	}
	return &Services{Handler: handler, Base: base, Env: env, JobEnv: jobEnv, Stop: func() { srv.Close(); closeAll() }}, nil
}

// ServeNetwork also serves on a docker network's gateway, for a job
// container, and returns the environment that points it there (see
// runner.Config.ContainerServices). It listens on that one address and
// accepts only the network's own subnet, so the services are not exposed on
// the host's other interfaces or to other docker networks; the token and the
// signed URLs guard them as on loopback. The links the services hand out are
// built on whichever address the request came in on.
func (s *Services) ServeNetwork(gateway netip.Addr, subnet netip.Prefix) (map[string]string, func(), error) {
	ln, err := net.Listen("tcp", netip.AddrPortFrom(gateway, 0).String())
	if err != nil {
		return nil, nil, err
	}
	base := "http://" + ln.Addr().String()
	srv := &http.Server{Handler: s.Handler, ReadHeaderTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return blob.WithBaseURL(context.Background(), base) }}
	go srv.Serve(local.OnlyFrom(ln, subnet))
	env := maps.Clone(s.Env)
	for k, v := range env {
		if v == s.Base+"/" { // ACTIONS_RESULTS_URL, ACTIONS_CACHE_URL
			env[k] = base + "/"
		}
	}
	return env, func() { srv.Close() }, nil
}

// freeBytes reports the space available to us on the disk holding dir.
func freeBytes(dir string) func() (int64, error) {
	return func() (int64, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return int64(st.Bavail) * int64(st.Bsize), nil
	}
}

// startCache serves the cache protocols. Entries live in bucket if one is
// given, else in a directory under dir that this process serves itself.
func startCache(mux *http.ServeMux, closers *[]func(), base, dir string, key []byte,
	signer *artifact.Signer, scope artifact.Scope, ttl time.Duration, v2 bool, limit int64, bucket blob.Store) (map[string]string, error) {
	var store blob.Store
	free := freeBytes(dir)
	if bucket != nil {
		// The bucket has no disk of ours to run out of; chunks still wait in
		// dir/spool until they are cut into parts.
		store, free = bucket, nil
	} else {
		d, err := blob.NewDir(filepath.Join(dir, "objects"), base, key)
		if err != nil {
			return nil, err
		}
		d.Prefix = "/_cache_dl/"
		store = d
		*closers = append(*closers, func() { d.Close() })
		mux.Handle(d.Prefix, d.Handler())
	}
	// Chunks wait next to the store, not in /tmp: a Go build cache alone is a gigabyte.
	svc, err := cache.New(cache.Config{Store: store, Signer: signer, BaseURL: base, IndexPath: filepath.Join(dir, "index.json"),
		SpoolDir: filepath.Join(dir, "spool"), AdvertiseV2: v2,
		MaxBytes: limit, Free: free})
	if err != nil {
		return nil, err
	}
	env, err := svc.Env(scope, ttl)
	if err != nil {
		svc.Close()
		return nil, err
	}
	*closers = append(*closers, svc.Close)
	svc.Routes(mux)
	return env, nil
}
