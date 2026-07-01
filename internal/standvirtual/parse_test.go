package standvirtual

import (
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// The listing fixture was captured for: make=audi, fuel=diesel,
// price 30000-60000, mileage<=50000.
func TestParseAdvertSearch(t *testing.T) {
	res, err := parseAdvertSearch(readFixture(t, "listing.html"))
	if err != nil {
		t.Fatalf("parseAdvertSearch: %v", err)
	}
	if res.TotalCount <= 0 {
		t.Fatalf("expected positive totalCount, got %d", res.TotalCount)
	}
	if len(res.Edges) == 0 {
		t.Fatal("expected at least one edge")
	}

	// Applied filters should echo the make and fuel we searched for.
	applied := map[string]string{}
	for _, f := range res.AppliedFilters {
		applied[f.Name] = f.Value
	}
	if applied["filter_enum_make"] != "audi" {
		t.Errorf("filter_enum_make = %q, want audi", applied["filter_enum_make"])
	}
	if applied["filter_enum_fuel_type"] != "diesel" {
		t.Errorf("filter_enum_fuel_type = %q, want diesel", applied["filter_enum_fuel_type"])
	}

	// Every offer should be a priced diesel Audi within the price window.
	for _, e := range res.Edges {
		o := e.Node.toOffer()
		if o.ID == "" || o.URL == "" || o.Title == "" {
			t.Errorf("offer missing core field: %+v", o)
		}
		if got := o.Fuel(); got != "Diesel" {
			t.Errorf("offer %s fuel = %q, want Diesel", o.ID, got)
		}
		if p, ok := o.Price(); !ok {
			t.Errorf("offer %s has no price", o.ID)
		} else if p < 30000 || p > 60000 {
			t.Errorf("offer %s price %d outside 30000-60000", o.ID, p)
		}
	}
}

func TestOfferHelpers(t *testing.T) {
	res, err := parseAdvertSearch(readFixture(t, "listing.html"))
	if err != nil {
		t.Fatalf("parseAdvertSearch: %v", err)
	}
	o := res.Edges[0].Node.toOffer()

	if o.Make() != "Audi" {
		t.Errorf("Make() = %q, want Audi", o.Make())
	}
	if o.PriceLabel() == "" || !strings.HasSuffix(o.PriceLabel(), "€") {
		t.Errorf("PriceLabel() = %q, want a EUR label", o.PriceLabel())
	}
	if o.LocationLabel() == "" {
		t.Error("LocationLabel() empty")
	}
	if o.SpecLabel() == "" {
		t.Error("SpecLabel() empty")
	}
}

func TestBuildURL(t *testing.T) {
	i := func(n int) *int { return &n }
	p := SearchParams{
		Make: "bmw", Model: "serie-3", FuelType: "diesel",
		MinPrice: i(5000), MaxPrice: i(20000), MaxKm: i(150000),
	}
	got := buildURL(p, 2)
	for _, want := range []string{
		"search%5Bfilter_enum_make%5D=bmw",
		"search%5Bfilter_enum_model%5D=serie-3",
		"search%5Bfilter_enum_fuel_type%5D=diesel",
		"search%5Bfilter_float_price%3Afrom%5D=5000",
		"search%5Bfilter_float_price%3Ato%5D=20000",
		"search%5Bfilter_float_mileage%3Ato%5D=150000",
		"search%5Border%5D=created_at%3Adesc",
		"page=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("buildURL missing %q in:\n%s", want, got)
		}
	}
	// Page 1 must not carry a page param.
	if strings.Contains(buildURL(p, 1), "page=") {
		t.Error("page 1 URL should not contain a page param")
	}
	// Unset mileage-from must be absent.
	if strings.Contains(got, "mileage%3Afrom") {
		t.Error("unset MinKm should not appear in URL")
	}
}

func TestExtractDetails(t *testing.T) {
	det, err := extractDetails(readFixture(t, "ad.html"))
	if err != nil {
		t.Fatalf("extractDetails: %v", err)
	}

	if len(det.Photos) == 0 {
		t.Fatal("expected photos from ad fixture")
	}
	seen := map[string]struct{}{}
	for _, p := range det.Photos {
		if !strings.Contains(p.URL, "apollo.olxcdn.com") {
			t.Errorf("unexpected photo URL %q", p.URL)
		}
		if strings.Contains(p.URL, ";s=") {
			t.Errorf("photo URL should be size-free base, got %q", p.URL)
		}
		if _, dup := seen[p.URL]; dup {
			t.Errorf("duplicate photo URL %q", p.URL)
		}
		seen[p.URL] = struct{}{}
	}
	if len(det.Photos) > maxPhotos {
		t.Errorf("photos %d exceeds cap %d", len(det.Photos), maxPhotos)
	}

	// Sized appends a resize suffix onto the base URL.
	if s := det.Photos[0].Sized(1200); !strings.HasSuffix(s, ";s=1200x0;q=80") {
		t.Errorf("Sized() = %q, want ;s=1200x0;q=80 suffix", s)
	}

	// Description is cleaned to plain text (no HTML tags, real content).
	if det.Description == "" {
		t.Fatal("expected a description")
	}
	if strings.Contains(det.Description, "<p>") || strings.Contains(det.Description, "<br") {
		t.Errorf("description still contains HTML tags: %q", det.Description[:80])
	}
	if !strings.Contains(det.Description, "Peugeot") {
		t.Errorf("description missing expected text: %q", det.Description[:80])
	}
}

func TestCleanDescription(t *testing.T) {
	in := `<p>Line one</p><p><br></p><p>Line &amp; two</p>Line three`
	got := cleanDescription(in)
	want := "Line one\n\nLine & two\nLine three"
	if got != want {
		t.Errorf("cleanDescription = %q, want %q", got, want)
	}
	if cleanDescription("") != "" {
		t.Error("empty description should stay empty")
	}
}

func TestFilterStateValues(t *testing.T) {
	body := readFixture(t, "make.html") // captured from /carros/bmw

	makes, err := filterStateValues(body, "filter_enum_make", "")
	if err != nil {
		t.Fatalf("makes: %v", err)
	}
	if !containsSlug(makes, "bmw") || !containsSlug(makes, "audi") {
		t.Errorf("make list missing bmw/audi (got %d makes)", len(makes))
	}

	models, err := filterStateValues(body, "filter_enum_model", "bmw")
	if err != nil {
		t.Fatalf("bmw models: %v", err)
	}
	if !containsSlug(models, "serie-3") {
		t.Errorf("bmw model list missing serie-3 (got %d models)", len(models))
	}
}

func containsSlug(vals []FilterValue, slug string) bool {
	for _, v := range vals {
		if v.Slug == slug {
			return true
		}
	}
	return false
}
