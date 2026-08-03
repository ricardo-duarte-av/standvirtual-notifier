package standvirtual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// listingBaseURL is the cars search page; filters go in the query string.
	listingBaseURL = "https://www.standvirtual.com/carros"
	// pageSize is how many organic results Standvirtual returns per page.
	pageSize  = 32
	userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0"
	// maxAttempts is how many times a fetch-and-parse cycle is tried before
	// giving up.
	maxAttempts = 3
)

// baseBackoff is the wait before the second attempt, doubled for each further
// one. A variable so tests can shorten it.
var baseBackoff = 3 * time.Second

// Client fetches and parses Standvirtual car listing pages.
type Client struct {
	http *http.Client
}

// NewClient returns a Client with a sane default timeout.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 30 * time.Second}}
}

// Search fetches all offers matching params, paginating up to maxPages and
// deduplicating by offer ID. Results come back newest-first.
func (c *Client) Search(ctx context.Context, p SearchParams, maxPages int) ([]Offer, error) {
	if maxPages < 1 {
		maxPages = 1
	}

	seen := make(map[string]struct{})
	var out []Offer

	for page := 1; page <= maxPages; page++ {
		res, err := c.fetchSearch(ctx, p, page)
		if err != nil {
			return nil, err
		}

		for _, e := range res.Edges {
			o := e.Node.toOffer()
			if o.ID == "" {
				continue
			}
			if _, dup := seen[o.ID]; dup {
				continue
			}
			seen[o.ID] = struct{}{}
			out = append(out, o)
		}

		// Stop when this page was empty or we've covered every organic result.
		if len(res.Edges) == 0 || page*pageSize >= res.TotalCount {
			break
		}
	}

	return out, nil
}

// fetchSearch fetches one results page and returns the parsed advertSearch block.
func (c *Client) fetchSearch(ctx context.Context, p SearchParams, page int) (*advertSearch, error) {
	var res *advertSearch
	err := c.fetchParsed(ctx, buildURL(p, page), func(body []byte) error {
		var err error
		res, err = parseAdvertSearch(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ErrTransient marks failures that are worth retrying: network hiccups, 5xx or
// 429 responses, and pages served without the state we expect (Standvirtual
// intermittently returns a placeholder or challenge page under load).
var ErrTransient = errors.New("transient standvirtual failure")

// transient tags err as retryable while keeping its message and wrapped errors.
func transient(err error) error { return transientErr{err} }

type transientErr struct{ err error }

func (e transientErr) Error() string        { return e.err.Error() }
func (e transientErr) Unwrap() error        { return e.err }
func (e transientErr) Is(target error) bool { return target == ErrTransient }

// fetchParsed fetches reqURL and hands the body to parse, retrying the whole
// fetch-and-parse cycle with exponential backoff while the failure is transient
// and ctx is alive. The last error is returned once the attempts run out.
func (c *Client) fetchParsed(ctx context.Context, reqURL string, parse func([]byte) error) error {
	var err error
	for attempt := 1; ; attempt++ {
		err = func() error {
			body, ferr := c.getHTML(ctx, reqURL)
			if ferr != nil {
				return ferr
			}
			return parse(body)
		}()
		if err == nil || attempt == maxAttempts || !errors.Is(err, ErrTransient) {
			return err
		}

		// Back off with jitter so retries do not line up across searches.
		wait := baseBackoff << (attempt - 1)
		wait += time.Duration(rand.Int63n(int64(wait / 2)))
		log.Printf("standvirtual: %v; retrying in %s (attempt %d/%d)",
			err, wait.Round(time.Millisecond), attempt+1, maxAttempts)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

// getHTML performs a GET with a browser UA and returns the response body.
func (c *Client) getHTML(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := c.http.Do(req)
	if err != nil {
		// Timeouts, DNS and TLS failures are all worth another go.
		return nil, transient(fmt.Errorf("standvirtual request: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("standvirtual returned HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, transient(err)
		}
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20)) // 12 MiB cap
	if err != nil {
		return nil, transient(fmt.Errorf("standvirtual body: %w", err))
	}
	return body, nil
}

// buildURL constructs a cars search URL for the given params and page.
func buildURL(p SearchParams, page int) string {
	q := url.Values{}
	q.Set("search[order]", "created_at:desc")
	if p.Make != "" {
		q.Set("search[filter_enum_make]", p.Make)
	}
	if p.Model != "" {
		q.Set("search[filter_enum_model]", p.Model)
	}
	if p.FuelType != "" {
		q.Set("search[filter_enum_fuel_type]", p.FuelType)
	}
	if p.MinPrice != nil {
		q.Set("search[filter_float_price:from]", strconv.Itoa(*p.MinPrice))
	}
	if p.MaxPrice != nil {
		q.Set("search[filter_float_price:to]", strconv.Itoa(*p.MaxPrice))
	}
	if p.MinKm != nil {
		q.Set("search[filter_float_mileage:from]", strconv.Itoa(*p.MinKm))
	}
	if p.MaxKm != nil {
		q.Set("search[filter_float_mileage:to]", strconv.Itoa(*p.MaxKm))
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	return listingBaseURL + "?" + q.Encode()
}

// nextDataRe captures the JSON payload embedded in the Next.js __NEXT_DATA__ tag.
var nextDataRe = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(.*?)</script>`)

// advertSearch is the parsed listing block: totals, applied filters and results.
type advertSearch struct {
	TotalCount     int             `json:"totalCount"`
	AppliedFilters []appliedFilter `json:"appliedFilters"`
	Edges          []edge          `json:"edges"`
}

type appliedFilter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type edge struct {
	Node node `json:"node"`
}

type node struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
	Price     struct {
		Amount struct {
			Units        int    `json:"units"`
			CurrencyCode string `json:"currencyCode"`
		} `json:"amount"`
	} `json:"price"`
	Location struct {
		City   struct{ Name string } `json:"city"`
		Region struct{ Name string } `json:"region"`
	} `json:"location"`
	Parameters []struct {
		Key          string `json:"key"`
		Value        string `json:"value"`
		DisplayValue string `json:"displayValue"`
	} `json:"parameters"`
}

func (n node) toOffer() Offer {
	o := Offer{
		ID:           n.ID,
		Title:        n.Title,
		CreatedAt:    n.CreatedAt,
		URL:          n.URL,
		PriceUnits:   n.Price.Amount.Units,
		CurrencyCode: n.Price.Amount.CurrencyCode,
		City:         n.Location.City.Name,
		Region:       n.Location.Region.Name,
		Params:       make(map[string]Param, len(n.Parameters)),
	}
	for _, p := range n.Parameters {
		o.Params[p.Key] = Param{Value: p.Value, DisplayValue: p.DisplayValue}
	}
	return o
}

// nextDataBlobs extracts the unwrapped urqlState "data" payloads from a page's
// __NEXT_DATA__ script. Each urqlState entry holds one GraphQL result under
// props.pageProps.urqlState[<hash>].data, where "data" is either a JSON object
// or a JSON-encoded string; both forms are normalised here.
func nextDataBlobs(html []byte) ([]json.RawMessage, error) {
	m := nextDataRe.FindSubmatch(html)
	if m == nil {
		return nil, transient(fmt.Errorf("__NEXT_DATA__ not found on page"))
	}

	var nd struct {
		Props struct {
			PageProps struct {
				URQLState map[string]struct {
					Data json.RawMessage `json:"data"`
				} `json:"urqlState"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(m[1], &nd); err != nil {
		return nil, fmt.Errorf("decode __NEXT_DATA__: %w", err)
	}

	var out []json.RawMessage
	for _, entry := range nd.Props.PageProps.URQLState {
		if raw := unwrapJSON(entry.Data); len(raw) > 0 {
			out = append(out, raw)
		}
	}
	return out, nil
}

// parseAdvertSearch extracts the advertSearch block (totals, applied filters and
// result edges) from a listing page's __NEXT_DATA__ payload.
func parseAdvertSearch(html []byte) (*advertSearch, error) {
	blobs, err := nextDataBlobs(html)
	if err != nil {
		return nil, err
	}
	for _, raw := range blobs {
		var wrap struct {
			AdvertSearch *advertSearch `json:"advertSearch"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil {
			continue // not the results entry (e.g. the filters entry)
		}
		if wrap.AdvertSearch != nil {
			return wrap.AdvertSearch, nil
		}
	}
	return nil, transient(fmt.Errorf("advertSearch not found in page state"))
}

// unwrapJSON normalises a urqlState "data" value: it may be a JSON object, or a
// JSON string containing JSON. In the latter case it is unquoted once.
func unwrapJSON(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, `"`) {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil
		}
		return json.RawMessage(inner)
	}
	return raw
}
