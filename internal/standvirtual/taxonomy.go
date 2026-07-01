package standvirtual

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// FilterValue is one selectable brand or model: its filter slug, display name
// and how many listings currently match it.
type FilterValue struct {
	Slug  string
	Name  string
	Count int
}

// Makes returns the list of car brands Standvirtual offers, with listing counts.
func (c *Client) Makes(ctx context.Context) ([]FilterValue, error) {
	body, err := c.getHTML(ctx, listingBaseURL)
	if err != nil {
		return nil, err
	}
	vals, err := filterStateValues(body, "filter_enum_make", "")
	if err != nil {
		return nil, err
	}
	sortByName(vals)
	return vals, nil
}

// Models returns the models available for the given brand slug, with counts. The
// slug must be a valid make (as returned by Makes); an unknown make yields an
// empty list.
func (c *Client) Models(ctx context.Context, make string) ([]FilterValue, error) {
	body, err := c.getHTML(ctx, listingBaseURL+"/"+make)
	if err != nil {
		return nil, err
	}
	vals, err := filterStateValues(body, "filter_enum_model", make)
	if err != nil {
		return nil, err
	}
	sortByName(vals)
	return vals, nil
}

// filterStates mirrors the filters block embedded in a listing page, carrying
// per-filter option lists (optionally conditioned on another filter's value,
// e.g. the model list for a specific make).
type filterStates struct {
	Filters struct {
		States []filterState `json:"states"`
	} `json:"filters"`
}

type filterState struct {
	FilterID   string             `json:"filterId"`
	Conditions []filterCondition  `json:"conditions"`
	Values     []filterValueGroup `json:"values"`
}

type filterCondition struct {
	FilterID string `json:"filterId"`
	Value    string `json:"value"`
}

type filterValueGroup struct {
	Values []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Counter int    `json:"counter"`
	} `json:"values"`
}

// filterStateValues finds the filters.states entry for filterID whose condition
// on filter_enum_make equals makeCond (empty = the unconditioned state, used for
// the make list itself) and flattens its value groups.
func filterStateValues(html []byte, filterID, makeCond string) ([]FilterValue, error) {
	blobs, err := nextDataBlobs(html)
	if err != nil {
		return nil, err
	}
	for _, raw := range blobs {
		var fs filterStates
		if err := json.Unmarshal(raw, &fs); err != nil || len(fs.Filters.States) == 0 {
			continue
		}
		for _, st := range fs.Filters.States {
			if st.FilterID != filterID || !matchesMake(st.Conditions, makeCond) {
				continue
			}
			var out []FilterValue
			for _, g := range st.Values {
				for _, v := range g.Values {
					out = append(out, FilterValue{Slug: v.ID, Name: v.Name, Count: v.Counter})
				}
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("filter %q options not found on page", filterID)
}

// matchesMake reports whether the state's make condition equals want. When want
// is empty, the state must have no make condition (the top-level make list).
func matchesMake(conds []filterCondition, want string) bool {
	var got string
	for _, c := range conds {
		if c.FilterID == "filter_enum_make" {
			got = c.Value
		}
	}
	return got == want
}

func sortByName(v []FilterValue) {
	sort.SliceStable(v, func(i, j int) bool {
		return strings.ToLower(v[i].Name) < strings.ToLower(v[j].Name)
	})
}
