package poller

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/standvirtual"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/store"
)

// captureNotifier records everything Notify receives.
type captureNotifier struct {
	mu     sync.Mutex
	events []store.Event
}

func (c *captureNotifier) Notify(_ context.Context, _ store.Search, events []store.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, events...)
}

// TestLivePipeline drives the whole flow against the live site (client.Search →
// store seed → reconcile → new-ad event → photo fetch), i.e. everything except
// the Matrix send. Skipped with -short.
func TestLivePipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live pipeline in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := standvirtual.NewClient()
	i := func(n int) *int { return &n }
	params := standvirtual.SearchParams{Make: "bmw", Model: "serie-3", MaxPrice: i(20000)}

	offers, err := client.Search(ctx, params, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(offers) < 2 {
		t.Skipf("need >=2 live offers to exercise diffing, got %d", len(offers))
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	id, err := st.AddSearch(params, "@tester:example.org")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := st.GetSearch(id)

	// Seed silently with all offers except the first, so the first is "new" next.
	if evs, err := st.Reconcile(s, offers[1:]); err != nil || len(evs) != 0 {
		t.Fatalf("seed reconcile: events=%d err=%v", len(evs), err)
	}
	s, _, _ = st.GetSearch(id) // reload with Seeded=true

	events, err := st.Reconcile(s, offers)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != store.EventNew || events[0].Offer.ID != offers[0].ID {
		t.Fatalf("expected one EventNew for %s, got %+v", offers[0].ID, events)
	}

	// The captured notifier interface is what the poller feeds; sanity-check it.
	cn := &captureNotifier{}
	cn.Notify(ctx, s, events)
	if len(cn.events) != 1 {
		t.Fatalf("notifier captured %d events", len(cn.events))
	}

	// Fetch details for the new ad — the notification path depends on this.
	det, err := client.FetchDetails(ctx, events[0].Offer.URL)
	if err != nil {
		t.Fatalf("FetchDetails: %v", err)
	}
	if len(det.Photos) == 0 {
		t.Errorf("expected photos for ad %s (%s)", events[0].Offer.ID, events[0].Offer.URL)
	}
	if det.Description == "" {
		t.Errorf("expected a description for ad %s", events[0].Offer.ID)
	}
	t.Logf("new ad %s: %q %s — %d photos, %d-char description", events[0].Offer.ID,
		events[0].Offer.Title, events[0].Offer.PriceLabel(), len(det.Photos), len(det.Description))
}
