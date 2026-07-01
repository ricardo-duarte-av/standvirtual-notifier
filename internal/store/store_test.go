package store

import (
	"path/filepath"
	"testing"

	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/standvirtual"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func offer(id string, price int) standvirtual.Offer {
	return standvirtual.Offer{
		ID: id, Title: "Car " + id, URL: "https://x/" + id,
		PriceUnits: price, CurrencyCode: "EUR",
	}
}

func ptr(n int) *int { return &n }

func TestAddAndListSearch(t *testing.T) {
	st := openTemp(t)
	id, err := st.AddSearch(standvirtual.SearchParams{
		Make: "bmw", Model: "serie-3", FuelType: "diesel", MaxPrice: ptr(20000), MaxKm: ptr(150000),
	}, "@a:x")
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.GetSearch(id)
	if err != nil || !ok {
		t.Fatalf("GetSearch: ok=%v err=%v", ok, err)
	}
	if got.Make != "bmw" || got.Model != "serie-3" || got.FuelType != "diesel" {
		t.Errorf("stored search fields wrong: %+v", got)
	}
	if got.MaxPrice == nil || *got.MaxPrice != 20000 || got.MinPrice != nil {
		t.Errorf("price bounds wrong: %+v", got)
	}
	if got.MaxKm == nil || *got.MaxKm != 150000 {
		t.Errorf("km bounds wrong: %+v", got)
	}
	if !got.Enabled || got.Seeded {
		t.Errorf("new search should be enabled and unseeded: %+v", got)
	}
}

func TestReconcileSeedsSilently(t *testing.T) {
	st := openTemp(t)
	id, _ := st.AddSearch(standvirtual.SearchParams{Make: "bmw"}, "@a:x")
	s, _, _ := st.GetSearch(id)

	// First reconcile seeds: everything stored, no events.
	events, err := st.Reconcile(s, []standvirtual.Offer{offer("100", 10000), offer("200", 20000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("seeding should emit no events, got %d", len(events))
	}
	if n, _ := st.AdCount(id); n != 2 {
		t.Fatalf("expected 2 stored ads, got %d", n)
	}

	// Reload so Seeded=true is reflected.
	s, _, _ = st.GetSearch(id)
	if !s.Seeded {
		t.Fatal("search should be seeded after first reconcile")
	}
}

func TestReconcileNewAndPriceChange(t *testing.T) {
	st := openTemp(t)
	id, _ := st.AddSearch(standvirtual.SearchParams{Make: "bmw"}, "@a:x")
	s, _, _ := st.GetSearch(id)

	// Seed with one ad.
	if _, err := st.Reconcile(s, []standvirtual.Offer{offer("100", 10000)}); err != nil {
		t.Fatal(err)
	}
	s, _, _ = st.GetSearch(id)

	// Now: ad 100 changed price, ad 200 is new.
	events, err := st.Reconcile(s, []standvirtual.Offer{offer("100", 9000), offer("200", 20000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}

	var sawNew, sawChange bool
	for _, e := range events {
		switch e.Type {
		case EventNew:
			sawNew = true
			if e.Offer.ID != "200" {
				t.Errorf("new event for wrong ad: %s", e.Offer.ID)
			}
		case EventPriceChange:
			sawChange = true
			if e.Offer.ID != "100" || e.OldPrice == nil || *e.OldPrice != 10000 {
				t.Errorf("price-change event wrong: id=%s old=%v", e.Offer.ID, e.OldPrice)
			}
			if p, _ := e.Offer.Price(); p != 9000 {
				t.Errorf("price-change new price = %d, want 9000", p)
			}
		}
	}
	if !sawNew || !sawChange {
		t.Errorf("missing events: new=%v change=%v", sawNew, sawChange)
	}

	// A third reconcile with no changes emits nothing.
	s, _, _ = st.GetSearch(id)
	events, err = st.Reconcile(s, []standvirtual.Offer{offer("100", 9000), offer("200", 20000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("stable reconcile should emit no events, got %d", len(events))
	}
}

func TestSetEnabledResetsSeeded(t *testing.T) {
	st := openTemp(t)
	id, _ := st.AddSearch(standvirtual.SearchParams{Make: "bmw"}, "@a:x")
	s, _, _ := st.GetSearch(id)
	st.Reconcile(s, []standvirtual.Offer{offer("100", 10000)}) // seeds

	if _, err := st.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetEnabled(id, true); err != nil {
		t.Fatal(err)
	}
	s, _, _ = st.GetSearch(id)
	if !s.Enabled || s.Seeded {
		t.Errorf("re-enabled search should be enabled and unseeded: %+v", s)
	}
}
