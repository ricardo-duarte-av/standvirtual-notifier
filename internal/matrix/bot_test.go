package matrix

import (
	"strings"
	"testing"

	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/standvirtual"
)

func TestNormalizeFuel(t *testing.T) {
	cases := map[string]string{
		"diesel":   "diesel",
		"gasoline": "gaz",
		"gasolina": "gaz",
		"petrol":   "gaz",
		"electric": "electric",
		"EV":       "electric",
		"plugin":   "plugin-hybrid",
		"phev":     "plugin-hybrid",
		"hybrid":   "hibride-gaz",
		"gpl":      "gpl",
	}
	for in, want := range cases {
		got, ok := normalizeFuel(in)
		if !ok || got != want {
			t.Errorf("normalizeFuel(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
	if _, ok := normalizeFuel("banana"); ok {
		t.Error("normalizeFuel(banana) should fail")
	}
}

func TestDescribeParams(t *testing.T) {
	i := func(n int) *int { return &n }
	got := describeParams(standvirtual.SearchParams{
		Make: "bmw", Model: "serie-3", FuelType: "diesel",
		MinPrice: i(5000), MaxPrice: i(20000), MaxKm: i(150000),
	})
	for _, want := range []string{"bmw", "serie-3", "diesel", "5000-20000€", "≤150000km"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeParams missing %q in %q", want, got)
		}
	}

	// Make-only search renders just the make.
	if got := describeParams(standvirtual.SearchParams{Make: "audi"}); got != "audi" {
		t.Errorf("describeParams(make only) = %q, want audi", got)
	}
}

func TestResolveSlug(t *testing.T) {
	vals := []standvirtual.FilterValue{
		{Slug: "508-sw", Name: "508 SW"},
		{Slug: "serie-3", Name: "Série 3"},
		{Slug: "alfa-romeo", Name: "Alfa Romeo"},
	}
	cases := map[string]string{
		"508-sw":     "508-sw", // exact slug
		"508 SW":     "508-sw", // display name
		"508 sw":     "508-sw", // spaced, lowercased → slugified
		"Série 3":    "serie-3",
		"serie-3":    "serie-3",
		"alfa romeo": "alfa-romeo",
		"Alfa Romeo": "alfa-romeo",
	}
	for in, want := range cases {
		got, ok := resolveSlug(vals, in)
		if !ok || got != want {
			t.Errorf("resolveSlug(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
	if _, ok := resolveSlug(vals, "911"); ok {
		t.Error("resolveSlug(911) should fail")
	}
}

func TestTokenize(t *testing.T) {
	got, err := tokenize(`!sv add "gran tourer" 5000 -`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"!sv", "add", "gran tourer", "5000", "-"}
	if len(got) != len(want) {
		t.Fatalf("tokenize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := tokenize(`"oops`); err == nil {
		t.Error("expected error for unterminated quote")
	}
}
