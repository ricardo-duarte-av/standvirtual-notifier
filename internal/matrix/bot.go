// Package matrix implements the Matrix side of the daemon: it receives !sv
// commands in the configured room and posts new-ad / price-change notifications
// for Standvirtual car listings.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/config"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/standvirtual"
	"github.com/ricardo-duarte-av/standvirtual-notifier/internal/store"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Seeder lets the bot ask the poller to fetch a freshly added search right away
// (seeding it) so its first notifications don't wait two full intervals.
type Seeder interface {
	PollSearch(ctx context.Context, s store.Search)
}

// Bot is the Matrix client wrapper.
type Bot struct {
	client  *mautrix.Client
	store   *store.Store
	roomID  id.RoomID
	startTS int64 // ms; ignore messages older than this
	seeder  Seeder
	http    *http.Client
	sv      *standvirtual.Client

	taxoMu sync.Mutex
	makes  []standvirtual.FilterValue            // cached brand list (lazy)
	models map[string][]standvirtual.FilterValue // cached models per make (lazy)
}

// New builds a Bot from config, using token authentication.
func New(cfg *config.Config, st *store.Store) (*Bot, error) {
	client, err := mautrix.NewClient(cfg.Matrix.Homeserver, id.UserID(cfg.Matrix.UserID), cfg.Matrix.AccessToken)
	if err != nil {
		return nil, err
	}
	return &Bot{
		client:  client,
		store:   st,
		roomID:  id.RoomID(cfg.Matrix.RoomID),
		startTS: time.Now().UnixMilli(),
		http:    &http.Client{Timeout: 30 * time.Second},
		sv:      standvirtual.NewClient(),
		models:  make(map[string][]standvirtual.FilterValue),
	}, nil
}

// SetSeeder wires the poller in (broken out to avoid a construction cycle).
func (b *Bot) SetSeeder(s Seeder) { b.seeder = s }

// Run ensures the bot is in the configured room, registers the command handler
// and blocks on sync until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if err := b.ensureJoined(ctx); err != nil {
		return err
	}

	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		b.onMessage(ctx, evt)
	})

	return b.client.SyncWithContext(ctx)
}

// ensureJoined joins the configured room if the bot isn't already a member.
// The bot cannot function without the room, so a join failure is fatal.
func (b *Bot) ensureJoined(ctx context.Context) error {
	joined, err := b.client.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("list joined rooms: %w", err)
	}
	for _, r := range joined.JoinedRooms {
		if r == b.roomID {
			log.Printf("matrix: already in room %s", b.roomID)
			return nil
		}
	}

	log.Printf("matrix: joining room %s", b.roomID)
	if _, err := b.client.JoinRoom(ctx, b.roomID.String(), nil); err != nil {
		return fmt.Errorf("join room %s: %w", b.roomID, err)
	}
	return nil
}

func (b *Bot) onMessage(ctx context.Context, evt *event.Event) {
	// Only our room, not our own messages, and nothing from before startup.
	if evt.RoomID != b.roomID || evt.Sender == b.client.UserID || evt.Timestamp < b.startTS {
		return
	}
	body := strings.TrimSpace(evt.Content.AsMessage().Body)
	if !strings.HasPrefix(body, "!sv") {
		return
	}
	b.handleCommand(ctx, evt.Sender, evt.ID, body)
}

func (b *Bot) handleCommand(ctx context.Context, sender id.UserID, replyTo id.EventID, body string) {
	args, err := tokenize(body)
	if err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	// args[0] == "!sv"
	if len(args) < 2 {
		b.replyCode(ctx, replyTo, helpText())
		return
	}

	switch strings.ToLower(args[1]) {
	case "add":
		b.cmdAdd(ctx, sender, replyTo, args[2:])
	case "list", "ls":
		b.cmdList(ctx, sender, replyTo)
	case "delete", "remove", "rm", "del":
		b.cmdDelete(ctx, sender, replyTo, args[2:])
	case "disable":
		b.cmdSetEnabled(ctx, sender, replyTo, args[2:], false)
	case "enable":
		b.cmdSetEnabled(ctx, sender, replyTo, args[2:], true)
	case "makes", "make", "brands":
		b.cmdMakes(ctx, replyTo, args[2:])
	case "models", "model":
		b.cmdModels(ctx, replyTo, args[2:])
	case "fuels", "fuel":
		b.reply(ctx, replyTo, "Fuel types: "+strings.Join(fuelSlugs(), ", "))
	case "help":
		b.replyCode(ctx, replyTo, helpText())
	default:
		b.replyCode(ctx, replyTo, helpText())
	}
}

func (b *Bot) cmdAdd(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string) {
	// add <make> <model|-> <minPrice|-> <maxPrice|-> <minKm|-> <maxKm|-> <fuel|->
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" || args[0] == "-" {
		b.reply(ctx, replyTo, "Usage: !sv add <make> <model> <minPrice> <maxPrice> <minKm> <maxKm> <fuel>  (use - to skip; make is required)")
		return
	}
	sp := standvirtual.SearchParams{Make: strings.ToLower(args[0])}

	sp.Model = optSlug(arg(args, 1))

	var perr error
	if sp.MinPrice, perr = optIntArg(args, 2); perr == nil {
		sp.MaxPrice, perr = optIntArg(args, 3)
	}
	if perr == nil {
		sp.MinKm, perr = optIntArg(args, 4)
	}
	if perr == nil {
		sp.MaxKm, perr = optIntArg(args, 5)
	}
	if perr != nil {
		b.reply(ctx, replyTo, "❌ invalid number: "+perr.Error())
		return
	}

	if fuel := arg(args, 6); fuel != "" && fuel != "-" {
		slug, ok := normalizeFuel(fuel)
		if !ok {
			b.reply(ctx, replyTo, fmt.Sprintf("❌ unknown fuel %q. Valid: %s", fuel, strings.Join(fuelSlugs(), ", ")))
			return
		}
		sp.FuelType = slug
	}

	// Resolve make/model against the live Standvirtual taxonomy, accepting either
	// the slug or the display name, and normalise sp to canonical slugs.
	if err := b.resolveSearch(ctx, &sp); err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}

	id, err := b.store.AddSearch(sp, sender.String())
	if err != nil {
		b.reply(ctx, replyTo, "❌ could not add search: "+err.Error())
		return
	}
	b.reply(ctx, replyTo, fmt.Sprintf("✅ Added search #%d: %s", id, describeParams(sp)))

	// Seed it now so we have a baseline; seeding emits no notifications.
	if b.seeder != nil {
		go b.seeder.PollSearch(context.Background(), store.Search{
			ID: id, Make: sp.Make, Model: sp.Model, FuelType: sp.FuelType,
			MinPrice: sp.MinPrice, MaxPrice: sp.MaxPrice, MinKm: sp.MinKm, MaxKm: sp.MaxKm,
			Owner: sender.String(),
		})
	}
}

func (b *Bot) cmdList(ctx context.Context, sender id.UserID, replyTo id.EventID) {
	mod := b.isModerator(ctx, sender)

	var searches []store.Search
	var err error
	if mod {
		searches, err = b.store.ListSearches() // moderators see everyone's
	} else {
		searches, err = b.store.ListSearchesByOwner(sender.String())
	}
	if err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	if len(searches) == 0 {
		b.reply(ctx, replyTo, "No searches yet. Add one with !sv add <make> <model> <minPrice> <maxPrice> <minKm> <maxKm> <fuel>")
		return
	}

	var sb strings.Builder
	if mod {
		sb.WriteString("All searches:\n")
	} else {
		sb.WriteString("Your searches:\n")
	}
	for _, s := range searches {
		n, _ := b.store.AdCount(s.ID)
		state := "🟢"
		if !s.Enabled {
			state = "⏸️ disabled"
		} else if !s.Seeded {
			state = "🟢 seeding…"
		}
		fmt.Fprintf(&sb, "#%d [%s] — %s — %d ads", s.ID, state, describeParams(s.Params()), n)
		if mod {
			fmt.Fprintf(&sb, " — by %s", s.Owner)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("\nUse the #index with delete/disable/enable.")
	b.replyCode(ctx, replyTo, strings.TrimRight(sb.String(), "\n"))
}

func (b *Bot) cmdDelete(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string) {
	s, ok := b.resolveOwned(ctx, sender, replyTo, args, "delete")
	if !ok {
		return
	}
	if _, err := b.store.RemoveSearch(s.ID); err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	b.reply(ctx, replyTo, fmt.Sprintf("🗑️ Deleted search #%d and its stored results", s.ID))
}

func (b *Bot) cmdSetEnabled(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string, enabled bool) {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	s, ok := b.resolveOwned(ctx, sender, replyTo, args, verb)
	if !ok {
		return
	}
	if _, err := b.store.SetEnabled(s.ID, enabled); err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	if enabled {
		b.reply(ctx, replyTo, fmt.Sprintf("▶️ Enabled search #%d (re-baselining silently on next poll)", s.ID))
	} else {
		b.reply(ctx, replyTo, fmt.Sprintf("⏸️ Disabled search #%d (kept, but not searched until enabled)", s.ID))
	}
}

// taxonomyResultLimit caps how many make/model matches are shown.
const taxonomyResultLimit = 40

func (b *Bot) cmdMakes(ctx context.Context, replyTo id.EventID, args []string) {
	makes, err := b.getMakes(ctx)
	if err != nil {
		b.reply(ctx, replyTo, "❌ could not load makes: "+err.Error())
		return
	}
	term := strings.ToLower(strings.TrimSpace(strings.Join(args, " ")))
	matches := filterValues(makes, term)
	if len(matches) == 0 {
		b.reply(ctx, replyTo, fmt.Sprintf("No make matching %q.", term))
		return
	}
	b.replyValues(ctx, replyTo, "Makes (use the slug in !sv add):", matches, "!sv models <make>")
}

func (b *Bot) cmdModels(ctx context.Context, replyTo id.EventID, args []string) {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		b.reply(ctx, replyTo, "Usage: !sv models <make>")
		return
	}
	make := strings.ToLower(args[0])
	models, err := b.getModels(ctx, make)
	if err != nil {
		b.reply(ctx, replyTo, "❌ could not load models: "+err.Error())
		return
	}
	if len(models) == 0 {
		b.reply(ctx, replyTo, fmt.Sprintf("No models found for make %q (is the make slug correct? see !sv makes).", make))
		return
	}
	term := strings.ToLower(strings.TrimSpace(strings.Join(args[1:], " ")))
	matches := filterValues(models, term)
	b.replyValues(ctx, replyTo, fmt.Sprintf("Models for %s (use the slug in !sv add):", make), matches, "")
}

// replyValues renders a slug/name/count list as a monospace reply, dropping
// zero-count entries when there are plenty of live ones so the list stays useful.
func (b *Bot) replyValues(ctx context.Context, replyTo id.EventID, header string, vals []standvirtual.FilterValue, footer string) {
	// Prefer entries with listings; fall back to all if none have counts.
	live := vals[:0:0]
	for _, v := range vals {
		if v.Count > 0 {
			live = append(live, v)
		}
	}
	if len(live) > 0 {
		vals = live
	}

	truncated := false
	if len(vals) > taxonomyResultLimit {
		vals = vals[:taxonomyResultLimit]
		truncated = true
	}

	var sb strings.Builder
	sb.WriteString(header + "\n")
	for _, v := range vals {
		if v.Count > 0 {
			fmt.Fprintf(&sb, "%-24s %s (%d)\n", v.Slug, v.Name, v.Count)
		} else {
			fmt.Fprintf(&sb, "%-24s %s\n", v.Slug, v.Name)
		}
	}
	if truncated {
		sb.WriteString("… (more not shown — pass a term to filter)\n")
	}
	if footer != "" {
		sb.WriteString("\n" + footer)
	}
	b.replyCode(ctx, replyTo, strings.TrimRight(sb.String(), "\n"))
}

// filterValues returns the values whose slug or name contains term (all when
// term is empty), sorted with the closest matches first.
func filterValues(vals []standvirtual.FilterValue, term string) []standvirtual.FilterValue {
	if term == "" {
		return vals
	}
	var out []standvirtual.FilterValue
	for _, v := range vals {
		if strings.Contains(strings.ToLower(v.Slug), term) || strings.Contains(strings.ToLower(v.Name), term) {
			out = append(out, v)
		}
	}
	return out
}

// resolveSearch validates a search's make and model against Standvirtual's live
// taxonomy and rewrites them to canonical slugs. Users may pass either the slug
// (e.g. "508-sw") or the display name (e.g. "508 SW"), so we match on both. The
// site echoes any slug back in appliedFilters (even bogus ones, yielding zero
// results), so membership in the real make/model lists is the only reliable
// check; the bot's cache is reused.
func (b *Bot) resolveSearch(ctx context.Context, sp *standvirtual.SearchParams) error {
	if sp.Make == "" {
		return fmt.Errorf("make is required")
	}
	makes, err := b.getMakes(ctx)
	if err != nil {
		return fmt.Errorf("could not verify make: %w", err)
	}
	slug, ok := resolveSlug(makes, sp.Make)
	if !ok {
		return fmt.Errorf("unknown make %q (see !sv makes)", sp.Make)
	}
	sp.Make = slug

	if sp.Model != "" {
		models, err := b.getModels(ctx, sp.Make)
		if err != nil {
			return fmt.Errorf("could not verify model: %w", err)
		}
		slug, ok := resolveSlug(models, sp.Model)
		if !ok {
			return fmt.Errorf("unknown model %q for make %q (see !sv models %s)", sp.Model, sp.Make, sp.Make)
		}
		sp.Model = slug
	}
	return nil
}

// resolveSlug matches user input against a taxonomy list by slug or display
// name (case-insensitive) and returns the canonical slug. Since Standvirtual
// slugs are the lowercased, space-to-hyphen form of the name, the input is also
// tried in that slugified form — so "508 SW", "508 sw" and "508-sw" all resolve.
func resolveSlug(vals []standvirtual.FilterValue, input string) (string, bool) {
	in := strings.ToLower(strings.TrimSpace(input))
	slugged := strings.ReplaceAll(in, " ", "-")
	for _, v := range vals {
		if strings.ToLower(v.Slug) == in || strings.ToLower(v.Slug) == slugged || strings.ToLower(v.Name) == in {
			return v.Slug, true
		}
	}
	return "", false
}

// getMakes returns the brand list, fetching and caching it on first use.
func (b *Bot) getMakes(ctx context.Context) ([]standvirtual.FilterValue, error) {
	b.taxoMu.Lock()
	defer b.taxoMu.Unlock()
	if b.makes != nil {
		return b.makes, nil
	}
	makes, err := b.sv.Makes(ctx)
	if err != nil {
		return nil, err
	}
	b.makes = makes
	return makes, nil
}

// getModels returns a make's model list, fetching and caching it on first use.
func (b *Bot) getModels(ctx context.Context, make string) ([]standvirtual.FilterValue, error) {
	b.taxoMu.Lock()
	defer b.taxoMu.Unlock()
	if m, ok := b.models[make]; ok {
		return m, nil
	}
	models, err := b.sv.Models(ctx, make)
	if err != nil {
		return nil, err
	}
	b.models[make] = models
	return models, nil
}

// resolveOwned parses the index argument, looks up the search, and enforces that
// the sender either owns it or is a room moderator. It replies with the relevant
// error and returns ok=false when the caller should stop.
func (b *Bot) resolveOwned(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string, verb string) (store.Search, bool) {
	if len(args) < 1 {
		b.reply(ctx, replyTo, "Usage: !sv "+verb+" <index>")
		return store.Search{}, false
	}
	id64, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.reply(ctx, replyTo, "❌ invalid index: "+args[0])
		return store.Search{}, false
	}
	s, found, err := b.store.GetSearch(id64)
	if err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return store.Search{}, false
	}
	if !found {
		b.reply(ctx, replyTo, fmt.Sprintf("No search #%d", id64))
		return store.Search{}, false
	}
	if s.Owner != sender.String() && !b.isModerator(ctx, sender) {
		b.reply(ctx, replyTo, fmt.Sprintf("🚫 Search #%d belongs to someone else (moderators can manage any search)", id64))
		return store.Search{}, false
	}
	return s, true
}

// moderatorPowerLevel is the minimum room power level treated as a moderator.
const moderatorPowerLevel = 50

// isModerator reports whether the user has moderator power (PL >= 50) in the room.
// It fetches full room state via State so the m.room.create event is wired into
// the power levels; this makes GetUserLevel honour the room-v12 rule that the
// creator has implicit infinite power despite being absent from the users map.
// On any lookup error it fails closed (returns false).
func (b *Bot) isModerator(ctx context.Context, user id.UserID) bool {
	state, err := b.client.State(ctx, b.roomID)
	if err != nil {
		log.Printf("matrix: fetch room state: %v", err)
		return false
	}
	plEvt, ok := state[event.StatePowerLevels][""]
	if !ok || plEvt == nil {
		return false
	}
	return plEvt.Content.AsPowerLevels().GetUserLevel(user) >= moderatorPowerLevel
}

// photoMaxSide caps the longer edge of downloaded images.
const photoMaxSide = 1200

// Notify implements poller.Notifier: for each event it fetches the ad's photo
// gallery and posts the main photo with a caption (title, price, specs,
// location), then threads any extra photos as replies. Ads without photos fall
// back to a text message.
func (b *Bot) Notify(ctx context.Context, s store.Search, events []store.Event) {
	for _, e := range events {
		b.notifyOne(ctx, s, e)
	}
}

func (b *Bot) notifyOne(ctx context.Context, s store.Search, e store.Event) {
	mentions := ownerMentions(s.Owner) // only the first/main message pings the owner

	// Fetch the gallery + description from the ad page (best-effort).
	details, err := b.sv.FetchDetails(ctx, e.Offer.URL)
	if err != nil {
		log.Printf("matrix: fetch details for ad %s: %v", e.Offer.ID, err)
	}
	photos := details.Photos

	// Full caption includes the description; the short one drops it, used as a
	// fallback when the full caption exceeds Matrix's per-event size limit.
	plain, htmlBody := formatEvent(s, e, details.Description)
	plainShort, htmlShort := formatEvent(s, e, "")

	// No photos: plain text message, nothing to thread.
	if len(photos) == 0 {
		b.notifyText(ctx, plain, htmlBody, plainShort, htmlShort, mentions)
		return
	}

	// Main photo carries the caption, pings the owner, and becomes the thread root.
	rootID, err := b.sendImage(ctx, photos[0], plain, htmlBody, nil, mentions)
	if isTooLarge(err) {
		log.Printf("matrix: caption too large for ad %s, retrying without description", e.Offer.ID)
		rootID, err = b.sendImage(ctx, photos[0], plainShort, htmlShort, nil, mentions)
	}
	if err != nil {
		log.Printf("matrix: main photo for ad %s: %v", e.Offer.ID, err)
		// Fall back to text so the notification isn't lost.
		b.notifyText(ctx, plain, htmlBody, plainShort, htmlShort, mentions)
		return
	}

	// Remaining photos go into a thread under the main message (no extra pings).
	for i, p := range photos[1:] {
		rel := (&event.RelatesTo{}).SetThread(rootID, rootID)
		caption := fmt.Sprintf("Photo %d/%d", i+2, len(photos))
		if _, err := b.sendImage(ctx, p, caption, caption, rel, nil); err != nil {
			log.Printf("matrix: thread photo %d for ad %s: %v", i+2, e.Offer.ID, err)
		}
	}
}

// sendImage downloads a photo, uploads it to the homeserver and sends an m.image
// event with the given caption, optionally related (e.g. threaded) and mentioning
// users. It returns the sent event's ID.
func (b *Bot) sendImage(ctx context.Context, photo standvirtual.Photo, caption, captionHTML string, rel *event.RelatesTo, mentions *event.Mentions) (id.EventID, error) {
	data, mime, err := b.download(ctx, photo.Sized(photoMaxSide))
	if err != nil {
		return "", err
	}
	up, err := b.client.UploadBytesWithName(ctx, data, mime, "car.jpg")
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	content := event.MessageEventContent{
		MsgType:  event.MsgImage,
		Body:     caption,   // caption text (differs from FileName → treated as caption)
		FileName: "car.jpg", // file name
		URL:      up.ContentURI.CUString(),
		Info: &event.FileInfo{
			MimeType: mime,
			Size:     len(data),
		},
		RelatesTo: rel,
		Mentions:  mentions,
	}
	if captionHTML != "" {
		content.Format = event.FormatHTML
		content.FormattedBody = captionHTML
	}

	resp, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content)
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// download fetches an image, returning its bytes and detected content type.
func (b *Bot) download(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20 MiB cap
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}

// replyRelation builds an m.in_reply_to relation, or nil for a standalone message.
func replyRelation(replyTo id.EventID) *event.RelatesTo {
	if replyTo == "" {
		return nil
	}
	return (&event.RelatesTo{}).SetReplyTo(replyTo)
}

func (b *Bot) reply(ctx context.Context, replyTo id.EventID, text string) {
	content := event.MessageEventContent{
		MsgType:   event.MsgText,
		Body:      text,
		RelatesTo: replyRelation(replyTo),
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		log.Printf("matrix: send: %v", err)
	}
}

func (b *Bot) replyHTML(ctx context.Context, replyTo id.EventID, plain, htmlBody string, mentions *event.Mentions) {
	content := event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          plain,
		Format:        event.FormatHTML,
		FormattedBody: htmlBody,
		Mentions:      mentions,
		RelatesTo:     replyRelation(replyTo),
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		log.Printf("matrix: send html: %v", err)
	}
}

// notifyText sends a standalone HTML notification, retrying with the shorter
// (description-less) body if the full one is rejected for exceeding Matrix's
// per-event size limit, so an over-long description never loses the whole
// notification.
func (b *Bot) notifyText(ctx context.Context, plain, htmlBody, plainShort, htmlShort string, mentions *event.Mentions) {
	err := b.sendText(ctx, plain, htmlBody, mentions)
	if isTooLarge(err) {
		log.Printf("matrix: message too large, retrying without description")
		err = b.sendText(ctx, plainShort, htmlShort, mentions)
	}
	if err != nil {
		log.Printf("matrix: send html: %v", err)
	}
}

// sendText posts one standalone HTML message and returns any send error.
func (b *Bot) sendText(ctx context.Context, plain, htmlBody string, mentions *event.Mentions) error {
	content := event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          plain,
		Format:        event.FormatHTML,
		FormattedBody: htmlBody,
		Mentions:      mentions,
	}
	_, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content)
	return err
}

// isTooLarge reports whether a send failed because the event exceeded the
// homeserver's per-event size limit (Matrix caps a PDU at 64 KiB), signalled by
// the M_TOO_LARGE error code or an HTTP 413 status.
func isTooLarge(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mautrix.MTooLarge) {
		return true
	}
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.IsStatus(http.StatusRequestEntityTooLarge) {
		return true
	}
	return false
}

// replyCode sends monospace text as a reply, wrapping it in an HTML <pre><code>
// block so column alignment survives in clients that render formatted_body. The
// plain body is kept as the fallback for text-only clients.
func (b *Bot) replyCode(ctx context.Context, replyTo id.EventID, text string) {
	b.replyHTML(ctx, replyTo, text, "<pre><code>"+html.EscapeString(text)+"</code></pre>", nil)
}

// descriptionLimit caps how much of the ad description goes in the caption.
const descriptionLimit = 1500

// formatEvent renders an event as a (plain, html) caption containing the header
// line (make/model, price, specs, location), the ad link and a truncated
// description.
func formatEvent(s store.Search, e store.Event, description string) (string, string) {
	o := e.Offer
	loc := o.LocationLabel()

	// Trailing details: specs (year · km · fuel · power) and location.
	var details []string
	if spec := o.SpecLabel(); spec != "" {
		details = append(details, spec)
	}
	if loc != "" {
		details = append(details, loc)
	}
	detailSuffix := ""
	if len(details) > 0 {
		detailSuffix = " · " + strings.Join(details, " · ")
	}

	var headPlain, headHTML string
	switch e.Type {
	case store.EventPriceChange:
		oldLabel := "?"
		if e.OldPrice != nil {
			oldLabel = strconv.Itoa(*e.OldPrice) + " €"
		}
		newLabel := o.PriceLabel()
		arrow := "💶"
		if e.OldPrice != nil {
			if p, ok := o.Price(); ok && p < *e.OldPrice {
				arrow = "📉"
			} else if ok && p > *e.OldPrice {
				arrow = "📈"
			}
		}
		headPlain = fmt.Sprintf("%s [#%d] %s — %s → %s%s",
			arrow, s.ID, o.Title, oldLabel, newLabel, detailSuffix)
		headHTML = fmt.Sprintf("%s <b>[#%d]</b> <a href=%q>%s</a> — <s>%s</s> → <b>%s</b>%s",
			arrow, s.ID, o.URL, html.EscapeString(o.Title),
			html.EscapeString(oldLabel), html.EscapeString(newLabel), html.EscapeString(detailSuffix))

	default: // EventNew
		headPlain = fmt.Sprintf("🆕 [#%d] %s — %s%s", s.ID, o.Title, o.PriceLabel(), detailSuffix)
		headHTML = fmt.Sprintf("🆕 <b>[#%d]</b> <a href=%q>%s</a> — <b>%s</b>%s",
			s.ID, o.URL, html.EscapeString(o.Title), html.EscapeString(o.PriceLabel()),
			html.EscapeString(detailSuffix))
	}

	plain := headPlain + "\n" + o.URL
	htmlBody := headHTML

	if desc := truncate(description, descriptionLimit); desc != "" {
		plain += "\n\n" + desc
		htmlBody += "<br><br>" + strings.ReplaceAll(html.EscapeString(desc), "\n", "<br>")
	}

	// Ping the search owner at the end of the caption.
	if s.Owner != "" {
		plain += "\n\n🔔 " + s.Owner
		htmlBody += "<br><br>🔔 " + mentionHTML(s.Owner)
	}
	return plain, htmlBody
}

// truncate shortens s to at most n runes, appending an ellipsis if cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// mentionHTML renders a Matrix user-mention pill for the given user ID.
func mentionHTML(userID string) string {
	return fmt.Sprintf("<a href=\"https://matrix.to/#/%s\">%s</a>",
		html.EscapeString(userID), html.EscapeString(userID))
}

// ownerMentions builds the m.mentions payload that actually triggers a ping.
func ownerMentions(owner string) *event.Mentions {
	if owner == "" {
		return nil
	}
	return &event.Mentions{UserIDs: []id.UserID{id.UserID(owner)}}
}

// describeParams renders a search's filters as a compact one-line summary.
func describeParams(sp standvirtual.SearchParams) string {
	parts := []string{sp.Make}
	if sp.Model != "" {
		parts = append(parts, sp.Model)
	}
	if sp.FuelType != "" {
		parts = append(parts, sp.FuelType)
	}
	if r := rangeLabel(sp.MinPrice, sp.MaxPrice, "€"); r != "" {
		parts = append(parts, r)
	}
	if r := rangeLabel(sp.MinKm, sp.MaxKm, "km"); r != "" {
		parts = append(parts, r)
	}
	return strings.Join(parts, " · ")
}

// rangeLabel formats an optional min/max range with a unit suffix, or "" when
// both bounds are unset.
func rangeLabel(min, max *int, unit string) string {
	switch {
	case min != nil && max != nil:
		return fmt.Sprintf("%d-%d%s", *min, *max, unit)
	case min != nil:
		return fmt.Sprintf("≥%d%s", *min, unit)
	case max != nil:
		return fmt.Sprintf("≤%d%s", *max, unit)
	default:
		return ""
	}
}

// arg returns args[i] or "" when out of range.
func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// optSlug lowercases a slug argument, treating "" and "-" as "no value".
func optSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "-" {
		return ""
	}
	return s
}

// optIntArg parses an optional integer at args[i]. Empty, missing or "-" = none.
func optIntArg(args []string, i int) (*int, error) {
	return optInt(arg(args, i))
}

// optInt parses an optional integer argument. Empty or "-" means "no value".
func optInt(s string) (*int, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, fmt.Errorf("%q", s)
	}
	return &n, nil
}

// fuelAliases maps user-friendly fuel words to Standvirtual filter slugs.
var fuelAliases = map[string]string{
	"diesel":          "diesel",
	"gasoleo":         "diesel",
	"gasóleo":         "diesel",
	"gasoline":        "gaz",
	"gasolina":        "gaz",
	"petrol":          "gaz",
	"gas":             "gaz",
	"gaz":             "gaz",
	"electric":        "electric",
	"eletrico":        "electric",
	"elétrico":        "electric",
	"ev":              "electric",
	"gpl":             "gpl",
	"lpg":             "gpl",
	"gnc":             "gnc",
	"cng":             "gnc",
	"plugin":          "plugin-hybrid",
	"plugin-hybrid":   "plugin-hybrid",
	"plug-in":         "plugin-hybrid",
	"phev":            "plugin-hybrid",
	"hybrid":          "hibride-gaz",
	"hibride-gaz":     "hibride-gaz",
	"hybrid-gasoline": "hibride-gaz",
	"hibride-diesel":  "hibride-diesel",
	"hybrid-diesel":   "hibride-diesel",
	"hydrogen":        "hidrogen",
	"hidrogen":        "hidrogen",
}

// normalizeFuel maps a user fuel word to its Standvirtual slug.
func normalizeFuel(s string) (string, bool) {
	slug, ok := fuelAliases[strings.ToLower(strings.TrimSpace(s))]
	return slug, ok
}

// fuelSlugs returns the distinct canonical fuel slugs, sorted, for help text.
func fuelSlugs() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, slug := range fuelAliases {
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// tokenize splits a command line honoring double-quoted segments.
func tokenize(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	hadToken := false

	flush := func() {
		if hadToken || cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
			hadToken = false
		}
	}

	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			hadToken = true
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return tokens, nil
}

func helpText() string {
	return strings.Join([]string{
		"Standvirtual notifier commands:",
		`  !sv add <make> <model> <minPrice> <maxPrice> <minKm> <maxKm> <fuel>  — add a search (use - to skip; make required)`,
		"  !sv makes [term]        — list car brands (slugs) to use as <make>",
		"  !sv models <make> [term] — list models (slugs) for a brand",
		"  !sv fuels               — list valid fuel types",
		"  !sv list                — list searches with their #index and state",
		"  !sv disable <index>     — stop searching an entry (kept in the DB)",
		"  !sv enable <index>      — resume a disabled entry",
		"  !sv delete <index>      — permanently delete an entry and its results",
		"  !sv help                — show this help",
		"",
		`Example: !sv add bmw serie-3 5000 20000 - 150000 diesel`,
	}, "\n")
}
