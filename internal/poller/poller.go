// Package poller periodically queries Standvirtual for each stored search,
// reconciles the results against the store and forwards resulting events to a
// Notifier.
package poller

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/standvirtual"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/store"
)

// failThreshold is how many consecutive transient failures a search may hit
// before they are logged. Standvirtual regularly serves the odd 5xx or
// state-less page; the client already retries those, so isolated misses are
// noise rather than something to act on.
const failThreshold = 3

// Notifier receives events worth telling the user about.
type Notifier interface {
	Notify(ctx context.Context, s store.Search, events []store.Event)
}

// Poller ties the Standvirtual client, the store and a Notifier together on a
// timer.
type Poller struct {
	store    *store.Store
	client   *standvirtual.Client
	notifier Notifier
	interval time.Duration
	maxPages int

	mu    sync.Mutex
	fails map[int64]int // consecutive transient failures per search ID
}

// New builds a Poller.
func New(st *store.Store, client *standvirtual.Client, n Notifier, interval time.Duration, maxPages int) *Poller {
	return &Poller{
		store: st, client: client, notifier: n,
		interval: interval, maxPages: maxPages,
		fails: make(map[int64]int),
	}
}

// Run polls all searches immediately, then on every interval tick until ctx is
// cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.pollAll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollAll(ctx)
		}
	}
}

func (p *Poller) pollAll(ctx context.Context) {
	searches, err := p.store.ListSearches()
	if err != nil {
		log.Printf("poller: list searches: %v", err)
		return
	}

	var enabled []store.Search
	for _, s := range searches {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	log.Printf("poller: polling %d enabled search(es) of %d", len(enabled), len(searches))

	start := time.Now()
	for _, s := range enabled {
		if ctx.Err() != nil {
			return
		}
		p.pollOne(ctx, s)
	}
	log.Printf("poller: poll cycle done in %s, next in %s", time.Since(start).Round(time.Millisecond), p.interval)
}

// PollSearch runs a single poll for one search now. It is used to seed a search
// immediately after it is added, so the first real events arrive one interval
// later rather than after two.
func (p *Poller) PollSearch(ctx context.Context, s store.Search) {
	p.pollOne(ctx, s)
}

func (p *Poller) pollOne(ctx context.Context, s store.Search) {
	log.Printf("poller: search %d (%s): fetching", s.ID, label(s))

	start := time.Now()
	offers, err := p.client.Search(ctx, s.Params(), p.maxPages)
	if err != nil {
		p.recordFailure(s.ID, err)
		return
	}
	p.recordSuccess(s.ID)
	log.Printf("poller: search %d: %d offer(s) in %s", s.ID, len(offers), time.Since(start).Round(time.Millisecond))

	events, err := p.store.Reconcile(s, offers)
	if err != nil {
		log.Printf("poller: reconcile search %d: %v", s.ID, err)
		return
	}
	if len(events) == 0 {
		log.Printf("poller: search %d: no changes", s.ID)
		return
	}
	log.Printf("poller: search %d: %d event(s), notifying", s.ID, len(events))
	p.notifier.Notify(ctx, s, events)
}

// label renders a search's main filters for log lines.
func label(s store.Search) string {
	parts := []string{s.Make}
	if s.Model != "" {
		parts = append(parts, s.Model)
	}
	if s.FuelType != "" {
		parts = append(parts, s.FuelType)
	}
	return strings.Join(parts, "/")
}

// recordFailure counts a failed poll and logs it, except for transient failures
// that have not yet repeated failThreshold times in a row. Cancellation during
// shutdown is never logged.
func (p *Poller) recordFailure(id int64, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if !errors.Is(err, standvirtual.ErrTransient) {
		log.Printf("poller: search %d: %v", id, err)
		return
	}

	p.mu.Lock()
	p.fails[id]++
	n := p.fails[id]
	p.mu.Unlock()

	if n < failThreshold {
		log.Printf("poller: search %d: %v (transient, retrying next cycle)", id, err)
		return
	}
	log.Printf("poller: search %d: %v (%d consecutive failures)", id, err, n)
}

// recordSuccess clears the failure streak, reporting recovery if the streak had
// grown far enough to have been logged.
func (p *Poller) recordSuccess(id int64) {
	p.mu.Lock()
	n := p.fails[id]
	delete(p.fails, id)
	p.mu.Unlock()

	if n >= failThreshold {
		log.Printf("poller: search %d: recovered after %d consecutive failures", id, n)
	}
}
