# standvirtual-notifier

A long-lived Go daemon that watches [Standvirtual.com](https://www.standvirtual.com/)
car searches and posts **new listings** and **price changes** into a Matrix room.
Searches are managed at runtime with `!sv` commands in that room.

It is a sibling of `olx-notifier`, sharing the same store/poller/matrix design;
the data source and search options are car-specific (brand, model, price, km,
fuel type) since Standvirtual lists only cars.

## How it works

- Standvirtual is a Next.js site: each cars search page
  (`/carros?search[...]`) embeds the full result set as JSON in a
  `__NEXT_DATA__` script tag. The daemon fetches those pages (no auth), parses
  the embedded `advertSearch` data, and paginates/deduplicates per search.
- Stores every ad it has seen per search in SQLite. On each poll:
  - an ad not seen before → **new**;
  - a seen ad whose price changed → **price change**;
  - otherwise → ignored.
- A newly added search is **seeded silently** on its first poll (all current ads
  stored, no messages) so adding a search never floods the room.
- Runs a Matrix client (mautrix-go) in the same process for commands + output.

Each notification fetches the ad's **photo gallery** from its page and sends the
**main photo** with a caption (title, price, specs — year · km · fuel · power —
and location) plus a link. **Additional photos** are posted as replies **in a
thread** under that message. Ads with no photo fall back to text. Images are
re-uploaded to your homeserver's media repo so they render inline.

## Build

```sh
go build -o standvirtual-notifier .
```

## Configure

Copy `config.example.yaml` to `config.yaml` and fill in your Matrix homeserver,
user id, access token and room id. `config.yaml` and `*.db` are gitignored.

## Run

```sh
./standvirtual-notifier -config config.yaml
```

## Matrix commands

Send these in the configured room:

| Command | Description |
| --- | --- |
| `!sv add <make> <model> <minPrice> <maxPrice> <minKm> <maxKm> <fuel>` | Add a search. `make` is required; use `-` to skip any other filter. |
| `!sv makes [term]` | List car brands (slugs) to use as `<make>`; optional term filters the list. |
| `!sv models <make> [term]` | List models (slugs) for a brand. |
| `!sv fuels` | List valid fuel types. |
| `!sv list` | List searches with their `#index`, state, filters and ad counts. |
| `!sv disable <index>` | Stop searching an entry (kept in the DB). |
| `!sv enable <index>` | Resume a disabled entry (silently re-baselines on next poll). |
| `!sv delete <index>` | Permanently delete an entry and its stored results. |
| `!sv` / `!sv help` | Show the command list. |

`make`/`model` accept either the Standvirtual **slug** (e.g. `bmw`, `serie-3`,
`508-sw`) or the **display name** (e.g. `BMW`, `Série 3`, `508 SW`) as shown by
`!sv makes` / `!sv models <make>`; on `!sv add` they are resolved against
Standvirtual's live taxonomy to the canonical slug, so a typo is rejected with a
hint rather than silently matching nothing. **Names containing spaces must be
quoted**, e.g. `!sv add peugeot "508 SW" - 22000 - 120000 plugin-hybrid` or
equivalently `!sv add peugeot 508-sw - 22000 - 120000 plugin-hybrid`.

`fuel` accepts friendly words that map to Standvirtual slugs: `diesel`,
`gasoline`/`petrol` (→ `gaz`), `electric`, `plugin`/`phev` (→ `plugin-hybrid`),
`hybrid` (→ `hibride-gaz`), `hibride-diesel`, `gpl`, `gnc`, `hydrogen`.

Example — BMW Série 3 diesel between 5 000 € and 20 000 €, up to 150 000 km:

```
!sv add bmw serie-3 5000 20000 - 150000 diesel
```

The filters map to Standvirtual query params `filter_enum_make`,
`filter_enum_model`, `filter_float_price:from`/`:to`,
`filter_float_mileage:from`/`:to` and `filter_enum_fuel_type`.

### Multi-user

Each search records the Matrix user who added it. By default users only see and
manage their own searches; **room moderators** (power level ≥ 50) see and manage
everyone's. New-listing / price-change notifications **ping the search owner**.

## Tests

```sh
go test -short ./...                                # offline unit tests (uses HTML fixtures)
go test -run TestLive ./internal/standvirtual -v    # hits the live Standvirtual site
```
