// Package standvirtual is a small client for Standvirtual.com car listings. The
// site is a Next.js app whose listing pages embed the full search result set as
// JSON in a __NEXT_DATA__ <script> tag; this package fetches those pages and maps
// the embedded advertSearch data into a compact Offer type.
package standvirtual

import (
	"fmt"
	"strconv"
	"strings"
)

// SearchParams describes a single Standvirtual car search. Make/Model/FuelType
// are Standvirtual filter slugs (e.g. "bmw", "serie-3", "diesel"); empty means
// "no filter". Nil price/km pointers mean "no bound".
type SearchParams struct {
	Make     string
	Model    string
	FuelType string
	MinPrice *int
	MaxPrice *int
	MinKm    *int
	MaxKm    *int
}

// Offer is the subset of a Standvirtual listing we care about, mapped from one
// advertSearch edge node.
type Offer struct {
	ID           string
	URL          string
	Title        string
	CreatedAt    string // RFC3339, e.g. "2026-06-26T20:20:10Z"
	PriceUnits   int    // integer price amount (price.amount.units)
	CurrencyCode string // e.g. "EUR"
	City         string
	Region       string
	// Params holds the listing's car attributes keyed by parameter key
	// (make, model, fuel_type, mileage, engine_power, first_registration_year…).
	Params map[string]Param
}

// Param is one car attribute: its machine value plus the site's display string.
type Param struct {
	Value        string
	DisplayValue string
}

// Price returns the numeric price and whether one is present.
func (o Offer) Price() (int, bool) {
	if o.PriceUnits <= 0 {
		return 0, false
	}
	return o.PriceUnits, true
}

// PriceLabel returns a human-readable price like "14900 €" (falls back to the
// currency code for non-EUR listings), or "" when there is no price.
func (o Offer) PriceLabel() string {
	p, ok := o.Price()
	if !ok {
		return ""
	}
	switch o.CurrencyCode {
	case "EUR", "":
		return strconv.Itoa(p) + " €"
	default:
		return strconv.Itoa(p) + " " + o.CurrencyCode
	}
}

// LocationLabel returns "City, Region" (matching how Standvirtual shows it),
// omitting any missing part.
func (o Offer) LocationLabel() string {
	switch {
	case o.City != "" && o.Region != "":
		return o.City + ", " + o.Region
	case o.City != "":
		return o.City
	default:
		return o.Region
	}
}

// param returns the display value for a parameter key, or "".
func (o Offer) param(key string) string {
	if p, ok := o.Params[key]; ok {
		return p.DisplayValue
	}
	return ""
}

// Make returns the brand display name (e.g. "BMW").
func (o Offer) Make() string { return o.param("make") }

// Model returns the model display name (e.g. "216 Gran Tourer").
func (o Offer) Model() string { return o.param("model") }

// Fuel returns the fuel type display name (e.g. "Diesel").
func (o Offer) Fuel() string { return o.param("fuel_type") }

// Year returns the first registration year display value (e.g. "2019").
func (o Offer) Year() string { return o.param("first_registration_year") }

// Mileage returns the mileage display value (e.g. "99700 km").
func (o Offer) Mileage() string { return o.param("mileage") }

// Power returns the engine power display value (e.g. "116 cv").
func (o Offer) Power() string { return o.param("engine_power") }

// SpecLabel joins the non-empty car specs into a compact "year · km · fuel ·
// power" string for notification headers.
func (o Offer) SpecLabel() string {
	var parts []string
	for _, s := range []string{o.Year(), o.Mileage(), o.Fuel(), o.Power()} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// Photo is one listing image: an Apollo CDN base URL that accepts a size suffix.
type Photo struct {
	// URL is the base image URL (…/files/<token>/image), without a size suffix.
	URL string
}

// Sized returns a concrete image URL scaled so the longer side is at most
// maxSide (0 = the CDN default). Standvirtual images accept a ";s=<W>x0;q=<Q>"
// suffix where height 0 preserves aspect ratio.
func (p Photo) Sized(maxSide int) string {
	if maxSide <= 0 {
		return p.URL
	}
	return fmt.Sprintf("%s;s=%dx0;q=80", p.URL, maxSide)
}
