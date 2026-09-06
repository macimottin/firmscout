package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The container healthcheck is the only part of this binary a test can reach without
// standing up the whole service, and it is the part that was missing: Compose has
// probed `/firmscout healthcheck` since before this package existed, the subcommand did
// not, and the api container therefore reported unhealthy forever while answering every
// request correctly. Nothing failed loudly, because a failing Compose healthcheck on a
// service nothing depends on being *healthy* is invisible until somebody reads
// `docker compose ps`.

func TestHealthcheckSucceedsOnA200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probed %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	if err := healthcheck([]string{"--url=" + srv.URL + "/healthz"}); err != nil {
		t.Fatalf("healthcheck against a healthy server: %v", err)
	}
}

// A probe that treated any response as success would report a service healthy while it
// returned 503 to every caller, which is worse than having no probe: it converts an
// outage into a silent one.
func TestHealthcheckFailsOnANon200(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		err := healthcheck([]string{"--url=" + srv.URL + "/healthz"})
		srv.Close()
		if err == nil {
			t.Errorf("status %d was reported healthy", status)
		}
	}
}

func TestHealthcheckFailsWhenNothingIsListening(t *testing.T) {
	t.Parallel()
	// A port nothing holds. Closing the test server first is what guarantees that,
	// rather than picking a number and hoping.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	if err := healthcheck([]string{"--url=" + url + "/healthz", "--timeout=2s"}); err == nil {
		t.Fatal("a closed port was reported healthy")
	}
}

// The timeout is what stops a wedged server from holding the probe open until Compose's
// own timeout kills it, which would report the container unhealthy for the right reason
// by the wrong route and make the cause harder to read.
func TestHealthcheckHonoursItsTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() { close(release); srv.Close() }()

	start := time.Now()
	err := healthcheck([]string{"--url=" + srv.URL + "/healthz", "--timeout=200ms"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a server that never answered was reported healthy")
	}
	if elapsed > 3*time.Second {
		t.Errorf("probe took %s; the --timeout flag is not being applied", elapsed)
	}
}
