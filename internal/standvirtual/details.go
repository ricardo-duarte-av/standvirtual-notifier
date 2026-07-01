package standvirtual

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
)

// maxPhotos caps how many gallery images we forward to Matrix per ad.
const maxPhotos = 20

// Details is the extra information carried on an ad's own page (not in the
// search results): the full photo gallery and the free-text description.
type Details struct {
	Photos      []Photo
	Description string // cleaned to plain text
}

// FetchDetails loads an ad's page and returns its photo gallery and description.
// Both come from props.pageProps.advert in the page's __NEXT_DATA__ payload.
func (c *Client) FetchDetails(ctx context.Context, adURL string) (Details, error) {
	body, err := c.getHTML(ctx, adURL)
	if err != nil {
		return Details{}, err
	}
	return extractDetails(body)
}

// advertPage mirrors the ad-detail slice of a page's __NEXT_DATA__.
type advertPage struct {
	Props struct {
		PageProps struct {
			Advert struct {
				Images struct {
					Photos []struct {
						URL string `json:"url"`
					} `json:"photos"`
				} `json:"images"`
				Description string `json:"description"`
			} `json:"advert"`
		} `json:"pageProps"`
	} `json:"props"`
}

// extractDetails parses the advert block out of an ad page. Split out from
// FetchDetails so it can be tested against a saved page without a network call.
func extractDetails(body []byte) (Details, error) {
	m := nextDataRe.FindSubmatch(body)
	if m == nil {
		return Details{}, fmt.Errorf("__NEXT_DATA__ not found on ad page")
	}
	var page advertPage
	if err := json.Unmarshal(m[1], &page); err != nil {
		return Details{}, fmt.Errorf("decode ad page: %w", err)
	}
	adv := page.Props.PageProps.Advert

	seen := make(map[string]struct{})
	var photos []Photo
	for _, p := range adv.Images.Photos {
		if p.URL == "" {
			continue
		}
		if _, dup := seen[p.URL]; dup {
			continue
		}
		seen[p.URL] = struct{}{}
		photos = append(photos, Photo{URL: p.URL})
		if len(photos) >= maxPhotos {
			break
		}
	}

	return Details{Photos: photos, Description: cleanDescription(adv.Description)}, nil
}

var (
	descBrRe    = regexp.MustCompile(`(?i)<br\s*/?>`)
	descBlockRe = regexp.MustCompile(`(?i)</(p|div|li)\s*>`)
	descTagRe   = regexp.MustCompile(`<[^>]+>`)
	descBlankRe = regexp.MustCompile(`\n{3,}`)
)

// cleanDescription turns Standvirtual's HTML description (<p>/<br> markup,
// entities) into plain text: block/line-break tags become newlines, remaining
// tags are stripped, entities decoded, each line trimmed and runs of blank
// lines collapsed.
func cleanDescription(s string) string {
	if s == "" {
		return ""
	}
	s = descBrRe.ReplaceAllString(s, "\n")
	s = descBlockRe.ReplaceAllString(s, "\n")
	s = descTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = descBlankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
