package standvirtual

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// shortenBackoff keeps retry tests fast.
func shortenBackoff(t *testing.T) {
	t.Helper()
	old := baseBackoff
	baseBackoff = time.Millisecond
	t.Cleanup(func() { baseBackoff = old })
}

func TestFetchParsedRetriesTransientStatus(t *testing.T) {
	shortenBackoff(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient()
	var got string
	err := c.fetchParsed(context.Background(), srv.URL, func(b []byte) error {
		got = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("fetchParsed: %v", err)
	}
	if got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
	if hits != 3 {
		t.Errorf("server hits = %d, want 3", hits)
	}
}

// A page that came back without the expected embedded state is retried too.
func TestFetchParsedRetriesParseFailure(t *testing.T) {
	shortenBackoff(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("<html>no state here</html>"))
	}))
	defer srv.Close()

	c := NewClient()
	err := c.fetchParsed(context.Background(), srv.URL, func(b []byte) error {
		_, perr := parseAdvertSearch(b)
		return perr
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrTransient) {
		t.Errorf("error %v should be transient", err)
	}
	if hits != maxAttempts {
		t.Errorf("server hits = %d, want %d", hits, maxAttempts)
	}
}

func TestFetchParsedDoesNotRetryPermanentStatus(t *testing.T) {
	shortenBackoff(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient()
	err := c.fetchParsed(context.Background(), srv.URL, func([]byte) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrTransient) {
		t.Errorf("HTTP 404 should not be transient, got %v", err)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
}
