package standvirtual

import (
	"context"
	"testing"
	"time"
)

// These tests hit the live Standvirtual site; skip them with `go test -short`.

func TestLiveSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live search in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	i := func(n int) *int { return &n }
	c := NewClient()
	offers, err := c.Search(ctx, SearchParams{
		Make: "audi", FuelType: "diesel", MinPrice: i(30000), MaxPrice: i(60000), MaxKm: i(50000),
	}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(offers) == 0 {
		t.Fatal("expected at least one offer")
	}
	for _, o := range offers {
		if o.Fuel() != "Diesel" {
			t.Errorf("offer %s fuel = %q, want Diesel", o.ID, o.Fuel())
		}
	}
}

func TestLiveMakesModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live taxonomy in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := NewClient()
	makes, err := c.Makes(ctx)
	if err != nil {
		t.Fatalf("Makes: %v", err)
	}
	if !containsSlug(makes, "bmw") {
		t.Error("Makes should include bmw")
	}
	models, err := c.Models(ctx, "bmw")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !containsSlug(models, "serie-3") {
		t.Error("bmw Models should include serie-3")
	}
}
