// Package store persists searches and the ads seen for each of them in SQLite,
// and computes new/price-change events when reconciling fresh results.
package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/standvirtual"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Search is a stored car search definition.
type Search struct {
	ID       int64
	Make     string
	Model    string
	FuelType string
	MinPrice *int
	MaxPrice *int
	MinKm    *int
	MaxKm    *int
	Seeded   bool
	Enabled  bool
	Owner    string // Matrix user ID that added the search
}

// Params converts a stored Search into standvirtual.SearchParams.
func (s Search) Params() standvirtual.SearchParams {
	return standvirtual.SearchParams{
		Make:     s.Make,
		Model:    s.Model,
		FuelType: s.FuelType,
		MinPrice: s.MinPrice,
		MaxPrice: s.MaxPrice,
		MinKm:    s.MinKm,
		MaxKm:    s.MaxKm,
	}
}

// EventType distinguishes a brand-new ad from a price change.
type EventType int

const (
	// EventNew is a listing not previously seen for this search.
	EventNew EventType = iota
	// EventPriceChange is a previously seen listing whose price changed.
	EventPriceChange
)

// Event is something worth notifying about.
type Event struct {
	Type     EventType
	Offer    standvirtual.Offer
	OldPrice *int // set only for EventPriceChange
}

// Open opens (and migrates) the SQLite database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection keeps this small daemon free of lock contention.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS searches (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  make        TEXT NOT NULL,
  model       TEXT,
  fuel_type   TEXT,
  min_price   INTEGER,
  max_price   INTEGER,
  min_km      INTEGER,
  max_km      INTEGER,
  seeded      INTEGER NOT NULL DEFAULT 0,
  enabled     INTEGER NOT NULL DEFAULT 1,
  owner       TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS seen_ads (
  search_id  INTEGER NOT NULL,
  ad_id      TEXT NOT NULL,
  price      INTEGER,
  title      TEXT,
  url        TEXT,
  first_seen TEXT NOT NULL,
  last_seen  TEXT NOT NULL,
  PRIMARY KEY (search_id, ad_id),
  FOREIGN KEY (search_id) REFERENCES searches(id) ON DELETE CASCADE
);`
	_, err := s.db.Exec(schema)
	return err
}

// AddSearch inserts a new search owned by the given Matrix user and returns its id.
func (s *Store) AddSearch(sp standvirtual.SearchParams, owner string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO searches (make, model, fuel_type, min_price, max_price, min_km, max_km, seeded, owner, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		sp.Make, sp.Model, sp.FuelType,
		nullInt(sp.MinPrice), nullInt(sp.MaxPrice), nullInt(sp.MinKm), nullInt(sp.MaxKm),
		owner, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetSearch returns a single search by id, and whether it exists.
func (s *Store) GetSearch(id int64) (Search, bool, error) {
	row := s.db.QueryRow(`SELECT `+searchColumns+` FROM searches WHERE id = ?`, id)
	se, err := scanSearch(row)
	if err == sql.ErrNoRows {
		return Search{}, false, nil
	}
	if err != nil {
		return Search{}, false, err
	}
	return se, true, nil
}

// RemoveSearch deletes a search and (via cascade) its seen ads. It reports
// whether a row was actually removed.
func (s *Store) RemoveSearch(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM searches WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetEnabled enables or disables a search. Re-enabling also resets the seeded
// flag so the next poll silently re-baselines the search: ads posted while it
// was disabled are absorbed without a burst of notifications. It reports whether
// a row was affected.
func (s *Store) SetEnabled(id int64, enabled bool) (bool, error) {
	var res sql.Result
	var err error
	if enabled {
		res, err = s.db.Exec(`UPDATE searches SET enabled = 1, seeded = 0 WHERE id = ?`, id)
	} else {
		res, err = s.db.Exec(`UPDATE searches SET enabled = 0 WHERE id = ?`, id)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const searchColumns = `id, make, model, fuel_type, min_price, max_price, min_km, max_km, seeded, enabled, owner`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSearch reads one search row selected with searchColumns.
func scanSearch(r rowScanner) (Search, error) {
	var (
		se                     Search
		model, fuel            sql.NullString
		minP, maxP, minK, maxK sql.NullInt64
		seeded, enabled        int
	)
	if err := r.Scan(&se.ID, &se.Make, &model, &fuel, &minP, &maxP, &minK, &maxK,
		&seeded, &enabled, &se.Owner); err != nil {
		return Search{}, err
	}
	se.Model = model.String
	se.FuelType = fuel.String
	se.MinPrice = toPtr(minP)
	se.MaxPrice = toPtr(maxP)
	se.MinKm = toPtr(minK)
	se.MaxKm = toPtr(maxK)
	se.Seeded = seeded != 0
	se.Enabled = enabled != 0
	return se, nil
}

// ListSearches returns all stored searches ordered by id (used by the poller).
func (s *Store) ListSearches() ([]Search, error) {
	return s.querySearches(`SELECT ` + searchColumns + ` FROM searches ORDER BY id`)
}

// ListSearchesByOwner returns only the searches owned by the given user.
func (s *Store) ListSearchesByOwner(owner string) ([]Search, error) {
	return s.querySearches(`SELECT `+searchColumns+` FROM searches WHERE owner = ? ORDER BY id`, owner)
}

func (s *Store) querySearches(query string, args ...any) ([]Search, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Search
	for rows.Next() {
		se, err := scanSearch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// AdCount returns how many ads are stored for a search.
func (s *Store) AdCount(searchID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM seen_ads WHERE search_id = ?`, searchID).Scan(&n)
	return n, err
}

// Reconcile diffs the freshly fetched offers against what's stored for the
// search, updating the store and returning events to notify about. On the very
// first reconcile of a search (seeded=0) it stores everything silently and
// returns no events, so adding a search never floods the room.
func (s *Store) Reconcile(search Search, offers []standvirtual.Offer) ([]Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Load current prices for this search.
	existing := map[string]*int{}
	rows, err := tx.Query(`SELECT ad_id, price FROM seen_ads WHERE search_id = ?`, search.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var price sql.NullInt64
		if err := rows.Scan(&id, &price); err != nil {
			rows.Close()
			return nil, err
		}
		existing[id] = toPtr(price)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seeding := !search.Seeded
	now := time.Now().UTC().Format(time.RFC3339)
	var events []Event

	for _, o := range offers {
		price, hasPrice := o.Price()
		var newPrice *int
		if hasPrice {
			p := price
			newPrice = &p
		}

		old, seen := existing[o.ID]
		switch {
		case !seen:
			if err := upsertAd(tx, search.ID, o, newPrice, now, true); err != nil {
				return nil, err
			}
			if !seeding {
				events = append(events, Event{Type: EventNew, Offer: o})
			}
		case priceChanged(old, newPrice):
			if err := upsertAd(tx, search.ID, o, newPrice, now, false); err != nil {
				return nil, err
			}
			if !seeding {
				events = append(events, Event{Type: EventPriceChange, Offer: o, OldPrice: old})
			}
		default:
			// Unchanged: just bump last_seen.
			if _, err := tx.Exec(
				`UPDATE seen_ads SET last_seen = ? WHERE search_id = ? AND ad_id = ?`,
				now, search.ID, o.ID); err != nil {
				return nil, err
			}
		}
	}

	if seeding {
		if _, err := tx.Exec(`UPDATE searches SET seeded = 1 WHERE id = ?`, search.ID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

func upsertAd(tx *sql.Tx, searchID int64, o standvirtual.Offer, price *int, now string, insert bool) error {
	if insert {
		_, err := tx.Exec(
			`INSERT INTO seen_ads (search_id, ad_id, price, title, url, first_seen, last_seen)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			searchID, o.ID, nullInt(price), o.Title, o.URL, now, now)
		return err
	}
	_, err := tx.Exec(
		`UPDATE seen_ads SET price = ?, title = ?, url = ?, last_seen = ?
		 WHERE search_id = ? AND ad_id = ?`,
		nullInt(price), o.Title, o.URL, now, searchID, o.ID)
	return err
}

// priceChanged reports whether the price differs, treating "no price" as a
// distinct state from any numeric price.
func priceChanged(old, new *int) bool {
	if (old == nil) != (new == nil) {
		return true
	}
	if old == nil {
		return false
	}
	return *old != *new
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func toPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
