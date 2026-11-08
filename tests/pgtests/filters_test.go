//go:build postgres

package pgtests

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

type filterCase struct {
	name       string
	collection string // default "posts"
	filter     string
	params     dbx.Params
	// want is the number of matching records, asserted on both databases
	// (a filter that matches nothing everywhere would verify nothing)
	want int
	// wantErr: the filter must be rejected on both databases
	wantErr bool
	// gap documents a known SQLite/Postgres difference: the case runs on
	// SQLite only and shows up as a visible skip (see docs/datastores.md)
	gap string
}

func findIds(app core.App, c filterCase) ([]string, error) {
	col := c.collection
	if col == "" {
		col = "posts"
	}

	params := []dbx.Params{}
	if c.params != nil {
		params = append(params, c.params)
	}

	records, err := app.FindRecordsByFilter(col, c.filter, "", 0, 0, params...)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.Id)
	}
	slices.Sort(ids)

	return ids, nil
}

func runFilterCases(t *testing.T, cases []filterCase) {
	sqliteApp := newSQLiteApp(t)
	seedRecords(t, sqliteApp)
	pgApp := newApp(t)
	seedRecords(t, pgApp)

	for _, c := range cases {
		name := strings.NewReplacer(" ", "_", "/", "_").Replace(c.name)
		if c.gap != "" {
			name = "known_gap/" + name
		}

		t.Run(name, func(t *testing.T) {
			sqliteIds, sqliteErr := findIds(sqliteApp, c)

			if c.gap != "" {
				if sqliteErr != nil {
					t.Fatalf("the reference run (sqlite) failed: %v", sqliteErr)
				}

				pgIds, pgErr := findIds(pgApp, c)
				pgResult := shortIds(pgIds)
				if pgErr != nil {
					pgResult = "error: " + strings.SplitN(pgErr.Error(), "; failed query", 2)[0]
				}
				if pgErr == nil && slices.Equal(sqliteIds, pgIds) {
					t.Fatalf("this known gap no longer differs, remove it from the skip list: %s", c.filter)
				}

				t.Skipf("%s\n  filter:   %s\n  sqlite:   %s\n  postgres: %s", c.gap, c.filter, shortIds(sqliteIds), pgResult)
			}

			pgIds, pgErr := findIds(pgApp, c)

			if c.wantErr {
				if sqliteErr == nil || pgErr == nil {
					t.Fatalf("expected an error on both, sqlite=%v postgres=%v", sqliteErr, pgErr)
				}
				return
			}

			if sqliteErr != nil {
				t.Fatalf("sqlite: %v", sqliteErr)
			}
			if pgErr != nil {
				t.Fatalf("postgres: %v", pgErr)
			}

			if !slices.Equal(sqliteIds, pgIds) {
				t.Fatalf("result mismatch\n sqlite:   %s\n postgres: %s", shortIds(sqliteIds), shortIds(pgIds))
			}

			if c.want >= 0 && len(sqliteIds) != c.want {
				t.Fatalf("expected %d records on both, got %d: %s", c.want, len(sqliteIds), shortIds(sqliteIds))
			}
		})
	}
}

func shortIds(ids []string) string {
	short := make([]string, len(ids))
	for i, id := range ids {
		short[i] = strings.TrimLeft(strings.TrimPrefix(id, id[:min(3, len(id))]), "0")
	}
	return fmt.Sprintf("[%s]", strings.Join(short, " "))
}

func TestRecordFilters(t *testing.T) {
	runFilterCases(t, []filterCase{
		// ----- text -----
		{name: "text eq", filter: `title = "Zebra"`, want: 1},
		{name: "text eq is case sensitive", filter: `title = "zebra"`, want: 0},
		{name: "text neq", filter: `title != "Zebra"`, want: 7},
		{name: "text eq empty", filter: `title = ""`, want: 1},
		{name: "text neq empty", filter: `title != ""`, want: 7},
		{name: "text eq null", filter: `title = null`, want: 1},
		{name: "text neq null", filter: `title != null`, want: 7},
		{name: "text like", filter: `title ~ "world"`, want: 2},
		{name: "text like case insensitive ascii", filter: `title ~ "HELLO"`, want: 3},
		{name: "text not like", filter: `title !~ "hello"`, want: 5},
		{name: "text like wildcard", filter: `title ~ "He%ld"`, want: 2},
		{name: "text like escapes percent", filter: `title ~ "t_p"`, want: 0},
		{name: "text like literal percent", filter: `title ~ "post%"`, want: 0},
		{name: "text like literal underscore", filter: `title ~ "h_u"`, want: 0},
		{name: "text column like column", filter: `title ~ status`, want: 1},
		{name: "text value like column", filter: `"Hello World, again" ~ title`, want: 3},
		{name: "text gt", filter: `title > "Hello"`, want: 6},
		{name: "text gte lt", filter: `title >= "Hello World" && title < "Third"`, want: 2},
		{name: "text lte", filter: `title <= "Fourth_post%"`, want: 2},
		{name: "text column eq column", filter: `title = body`, want: 1},
		{name: "text column neq column", filter: `title != body`, want: 7},
		{name: "text lower modifier", filter: `title:lower = "zebra"`, want: 1},
		{name: "text lower modifier neq", filter: `title:lower != "hello world"`, want: 6},
		{name: "id eq", filter: `id = "pos000000000p1"`, want: 1},
		{name: "id like", filter: `id ~ "p1"`, want: 1},
		{name: "editor like", filter: `body ~ "first"`, want: 1},
		{name: "email like case", filter: `contact ~ "zed@example.com"`, want: 1},
		{name: "email eq", filter: `contact = "x@example.com"`, want: 1},
		{name: "url eq", filter: `site = "https://example.com"`, want: 1},
		{name: "url empty", filter: `site = ""`, want: 7},
		{name: "unicode like is ascii only", filter: `title ~ "école"`, want: 0},
		{name: "unicode like exact", filter: `title ~ "ÉCOLE"`, want: 1},
		{name: "unicode lower is ascii only", filter: `title:lower ~ "école"`, want: 0},

		// ----- number -----
		{name: "number eq", filter: `views = 10`, want: 2},
		{name: "number eq float", filter: `views = 5.5`, want: 1},
		{name: "number neq", filter: `views != 10`, want: 6},
		{name: "number gt", filter: `views > 10`, want: 2},
		{name: "number gte", filter: `views >= 10`, want: 4},
		{name: "number lt", filter: `views < 5.5`, want: 2},
		{name: "number lte", filter: `views <= 5.5`, want: 3},
		{name: "number zero", filter: `views = 0`, want: 1},
		{name: "number negative", filter: `views < 0`, want: 1},
		{name: "number range", filter: `views > 0 && views < 100`, want: 5},
		{name: "number eq string coercion", filter: `views = "10"`, want: 2},
		{name: "number gt string coercion", filter: `views > "10"`, want: 2},
		{name: "number column eq column", filter: `views = authors_via_rank.rank`, wantErr: true, want: -1},
		{name: "number eq param", filter: `views = {:v}`, params: dbx.Params{"v": 20}, want: 1},
		{name: "number like", filter: `views ~ "5"`, want: 1},
		{name: "number lower", filter: `views:lower > 0`, gap: "Postgres does not compare text with a number (LOWER of a number is text), SQLite orders text after numbers", want: -1},
		{name: "number eq null", filter: `views = null`, want: 0},
		{name: "number neq null", filter: `views != null`, want: 8},
		{name: "number relation field", filter: `author.rank > 1`, want: 3},
		{name: "number relation field eq", filter: `author.rank = 2.5`, want: 3},
		{name: "number vs relation number", filter: `views > author.rank`, want: 6},

		// ----- bool -----
		{name: "bool eq true", filter: `active = true`, want: 5},
		{name: "bool eq false", filter: `active = false`, want: 3},
		{name: "bool neq true", filter: `active != true`, want: 3},
		{name: "bool neq false", filter: `active != false`, want: 5},
		{name: "bool param", filter: `active = {:b}`, params: dbx.Params{"b": true}, want: 5},
		{name: "bool with and", filter: `active = true && views > 10`, want: 1},
		{name: "bool eq number", filter: `active = 1`, gap: "Postgres does not compare a boolean to a number", want: -1},
		{name: "bool column eq column", filter: `active = active`, want: 8},

		// ----- select / multi-select -----
		{name: "select eq", filter: `status = "published"`, want: 3},
		{name: "select neq", filter: `status != "published"`, want: 5},
		{name: "select empty", filter: `status = ""`, want: 1},
		{name: "select null", filter: `status = null`, want: 1},
		{name: "select like", filter: `status ~ "pub"`, want: 3},
		{name: "select length", filter: `status:length = 1`, want: 7},
		{name: "select length zero", filter: `status:length = 0`, want: 1},
		{name: "select each", filter: `status:each = "published"`, want: 3},
		{name: "multi select eq", filter: `labels = "a"`, want: 0},
		{name: "multi select neq", filter: `labels != "a"`, want: 8},
		{name: "multi select any eq", filter: `labels ?= "a"`, want: 0},
		{name: "multi select any neq", filter: `labels ?!= "a"`, want: 8},
		{name: "multi select multi neq", filter: `labels != "a" && labels != "b"`, want: 8},
		{name: "multi select any like", filter: `labels ?~ "c"`, want: 3},
		{name: "multi select like", filter: `labels ~ "c"`, want: 3},
		{name: "multi select not like", filter: `labels !~ "c"`, want: 5},
		{name: "multi select length", filter: `labels:length = 2`, want: 4},
		{name: "multi select length gt", filter: `labels:length > 1`, want: 5},
		{name: "multi select length zero", filter: `labels:length = 0`, want: 1},
		{name: "multi select each", filter: `labels:each = "b"`, want: 1},
		{name: "multi select each neq", filter: `labels:each != "b"`, want: 4},
		{name: "multi select each any", filter: `labels:each ?= "d"`, want: 2},
		{name: "multi select eq json array", filter: `labels = '["a","b"]'`, gap: "SQLite compares the raw JSON text of the array, which is formatted differently on Postgres (jsonb)", want: -1},
		{name: "multi select eq empty", filter: `labels = ""`, want: 0},
		{name: "multi select null", filter: `labels = null`, want: 0},
		{name: "multi select both", filter: `labels ?= "a" && labels ?= "b"`, want: 0},
		{name: "multi select param", filter: `labels ?= {:l}`, params: dbx.Params{"l": "d"}, want: 0},

		// ----- relations -----
		{name: "relation eq id", filter: `author = "aut000000000a1"`, want: 3},
		{name: "relation neq id", filter: `author != "aut000000000a1"`, want: 5},
		{name: "relation empty", filter: `author = ""`, want: 2},
		{name: "relation id path", filter: `author.id = "aut000000000a1"`, want: 3},
		{name: "relation field", filter: `author.name = "Alice"`, want: 3},
		{name: "relation field like", filter: `author.name ~ "ali"`, want: 3},
		{name: "relation field neq", filter: `author.name != "Alice"`, want: 5},
		{name: "relation null", filter: `author = null`, want: 2},
		{name: "relation field lower", filter: `author.name:lower = "bob"`, want: 3},
		{name: "multi relation eq", filter: `tags = "tag000000000t1"`, want: 0},
		{name: "multi relation any eq", filter: `tags ?= "tag000000000t1"`, want: 0},
		{name: "multi relation neq", filter: `tags != "tag000000000t1"`, want: 8},
		{name: "multi relation any neq", filter: `tags ?!= "tag000000000t1"`, want: 8},
		{name: "multi relation length", filter: `tags:length = 2`, want: 2},
		{name: "multi relation each", filter: `tags:each = "tag000000000t2"`, want: 1},
		{name: "multi relation field", filter: `tags.name = "go"`, want: 1},
		{name: "multi relation field any", filter: `tags.name ?= "go"`, want: 4},
		{name: "multi relation field like", filter: `tags.name ~ "rust"`, want: 1},
		{name: "multi relation field like literal", filter: `tags.name ~ "0%_s"`, want: 0},
		{name: "multi relation field neq", filter: `tags.name != "go"`, want: 4},
		{name: "multi relation field any neq", filter: `tags.name ?!= "go"`, want: 7},
		{name: "multi relation field and", filter: `tags.name ?= "go" && tags.name ?= "Rust"`, want: 0},
		{name: "multi relation null", filter: `tags = ""`, want: 0},
		{name: "relation chain", filter: `comments_via_post.post.author.name = "Alice"`, collection: "posts", want: 1},
		{name: "relation of other collection", collection: "comments", filter: `post.title = "Hello World"`, want: 2},
		{name: "relation of other collection multi", collection: "comments", filter: `seen.title ?= "Third post"`, want: 1},
		{name: "relation of other collection multi all", collection: "comments", filter: `seen.title = "Hello World"`, want: 0},

		// ----- back relations -----
		{name: "back relation single", filter: `comments_via_post.text = "nice"`, want: 0},
		{name: "back relation single like", filter: `comments_via_post.text ~ "nice"`, want: 1},
		{name: "back relation single any like", filter: `comments_via_post.text ?~ "nice"`, want: 1},
		{name: "back relation single empty", filter: `comments_via_post.text = ""`, want: 6},
		{name: "back relation single id", filter: `comments_via_post.id ?= "com000000000c3"`, want: 1},
		{name: "back relation multi", filter: `comments_via_seen.text ?= "nice"`, want: 2},
		{name: "back relation multi id", filter: `comments_via_seen.id = "com000000000c1"`, want: 2},
		{name: "back relation both", filter: `comments_via_post.text ?= "nice" && comments_via_seen.text ?= "nice"`, want: 1},
		{name: "back relation from author", collection: "authors", filter: `posts_via_author.title ?= "Hello World"`, want: 2},
		{name: "back relation from author count", collection: "authors", filter: `posts_via_author.views ?> 50`, want: 1},
		{name: "back relation from tags", collection: "tags", filter: `posts_via_tags.title ?= "Zebra"`, want: 3},
		{name: "back relation from tags nested", collection: "tags", filter: `posts_via_tags.comments_via_post.text ?= "meh"`, want: 1},

		// ----- json -----
		{name: "json number", filter: `meta.n = 5`, want: 1},
		{name: "json number gt", filter: `meta.n > 5`, gap: "SQLite orders JSON text values after numbers (meta.n \"5\" > 5), jsonb orders strings before numbers", want: -1},
		{name: "json number string mismatch", filter: `meta.n = "5"`, want: 1},
		{name: "json string", filter: `meta.s = "abc"`, want: 1},
		{name: "json string like", filter: `meta.s ~ "ab"`, want: 2},
		{name: "json string lower", filter: `meta.s:lower = "abc"`, want: 0},
		{name: "json bool", filter: `meta.b = true`, want: 1},
		{name: "json bool false", filter: `meta.b = false`, want: 0},
		{name: "json null", filter: `meta.n = null`, want: 5},
		{name: "json not null", filter: `meta.n != null`, want: 3},
		{name: "json json null value", filter: `meta.z = null`, want: 8},
		{name: "json nested", filter: `meta.o.k = "v"`, want: 1},
		{name: "json array index", filter: `meta.arr.1 = 2`, want: 1},
		{name: "json array index first", filter: `meta.arr.0 = 1`, want: 1},
		{name: "json missing", filter: `meta.missing = null`, want: 8},
		{name: "json root number", filter: `meta = 5`, want: 1},
		{name: "json root string", filter: `meta = "plain"`, want: 1},
		{name: "json root null", filter: `meta = null`, want: 1},
		{name: "json root not null", filter: `meta != null`, want: 7},
		{name: "json root like", filter: `meta ~ "plain"`, want: 1},
		{name: "json column eq column", filter: `meta.s = title`, want: 0},
		{name: "json param", filter: `meta.n = {:n}`, params: dbx.Params{"n": 7}, want: 1},
		{name: "json text param", filter: `meta.s = {:s}`, params: dbx.Params{"s": "x"}, want: 1},
		{name: "json geo lon", filter: `geo.lon > 5`, want: 2},
		{name: "json geo lat eq", filter: `geo.lat = 0`, want: 6},
		{name: "json any eq", filter: `meta.s ?= "abc"`, want: 1},
		{name: "json neq", filter: `meta.s != "abc"`, want: 7},
		{name: "json relation path", collection: "comments", filter: `post.meta.n = 5`, want: 2},
		{name: "json relation path multi", collection: "comments", filter: `seen.meta.n ?= 7`, want: 1},
		{name: "json type ordering", filter: `meta.n >= 5 && meta.n <= 6`, want: 1},
		{name: "json object text", filter: `meta.o ~ "k"`, want: 1},
		{name: "json array root like", filter: `meta ~ "1,2"`, gap: "SQLite searches the compact JSON text, Postgres jsonb renders arrays with spaces ([1, 2, 3])", want: -1},

		// ----- geoPoint, file, date -----
		{name: "geo point", filter: `geo.lat ~ "2"`, want: 2},
		{name: "geo distance", filter: `geoDistance(geo.lon, geo.lat, 10, 20) < 200`, want: 2},
		{name: "geo distance numbers", filter: `geoDistance(10, 20, 10, 20) < 1`, want: 8},
		{name: "geo distance far", filter: `geoDistance(geo.lon, geo.lat, 0, 0) > 1000`, want: 2},
		{name: "file eq", filter: `cover = "cover_one.png"`, want: 1},
		{name: "file empty", filter: `cover = ""`, want: 7},
		{name: "file like", filter: `cover ~ "cover"`, want: 1},
		{name: "files any eq", filter: `files ?= "f2_bbb.png"`, want: 0},
		{name: "files like", filter: `files ~ ".txt"`, want: 2},
		{name: "files length", filter: `files:length = 2`, want: 1},
		{name: "files each", filter: `files:each ~ "f"`, want: 2},
		{name: "files each like exact", filter: `files:each ?~ "ccc"`, want: 1},
		{name: "date gt", filter: `when > "2024-01-15 10:00:00.000Z"`, want: 3},
		{name: "date gte", filter: `when >= "2024-01-15 10:00:00.000Z"`, want: 4},
		{name: "date lt date only", filter: `when < "2024-01-16"`, want: 5},
		{name: "date eq", filter: `when = "2024-02-20 08:30:00.000Z"`, want: 1},
		{name: "date empty", filter: `when = ""`, want: 3},
		{name: "date not empty", filter: `when != ""`, want: 5},
		{name: "date like", filter: `when ~ "2024-0"`, want: 3},
		{name: "date vs autodate", filter: `when < created`, want: 8},
		{name: "date range", filter: `when >= "2024-01-01" && when < "2025-01-01"`, want: 3},
		{name: "macro now", filter: `created <= @now`, want: 8},
		{name: "macro now future", filter: `created > @now`, want: 0},
		{name: "macro year", filter: `when < @yearStart`, want: 8},
		{name: "macro todayStart", filter: `created >= @todayStart && created <= @todayEnd`, want: 8},
		{name: "macro todayEnd", filter: `when <= @todayEnd`, want: 8},
		{name: "macro month start", filter: `created >= @monthStart`, want: 8},
		{name: "macro weekday", filter: `@weekday >= 0`, want: 8},
		{name: "macro year number", filter: `@year > 2000`, want: 8},
		{name: "macro second", filter: `@second >= 0 && @minute >= 0 && @hour >= 0 && @day >= 1 && @month >= 1`, want: 8},
		{name: "macro now yesterday", filter: `@yesterday < @tomorrow`, want: 8},

		// ----- strftime -----
		{name: "strftime year", filter: `strftime('%Y', when) = '2024'`, want: 3},
		{name: "strftime year month", filter: `strftime('%Y-%m', when) = '2024-02'`, want: 1},
		{name: "strftime date parts", filter: `strftime('%Y-%m-%d', when) = '2024-01-15'`, want: 1},
		{name: "strftime time parts", filter: `strftime('%H:%M:%S', when) = '10:00:00'`, want: 1},
		{name: "strftime fraction", filter: `strftime('%f', when) = '59.999'`, want: 1},
		{name: "strftime dayofyear", filter: `strftime('%j', when) = '046'`, want: 0},
		{name: "strftime weekday", filter: `strftime('%w', when) = '1'`, want: 1},
		{name: "strftime epoch", filter: `strftime('%s', when) = '1705312800'`, want: 1},
		{name: "strftime start of month", filter: `strftime('%Y-%m-%d', when, 'start of month') = '2024-02-01'`, want: 1},
		{name: "strftime start of year", filter: `strftime('%Y-%m-%d', when, 'start of year') = '2025-01-01'`, want: 1},
		{name: "strftime start of day", filter: `strftime('%Y-%m-%d %H', when, 'start of day') = '2024-01-15 00'`, want: 1},
		{name: "strftime modifier days", filter: `strftime('%Y-%m-%d', when, '+20 days') = '2024-02-04'`, want: 1},
		{name: "strftime modifier months", filter: `strftime('%Y-%m', when, '+1 month') = '2024-02'`, want: 1},
		{name: "strftime modifier hours", filter: `strftime('%H', when, '+14 hours') = '00'`, want: 1},
		{name: "strftime modifier chain", filter: `strftime('%Y-%m-%d', when, 'start of month', '+1 month', '-1 day') = '2024-02-29'`, want: 1},
		{name: "strftime weekday modifier", filter: `strftime('%Y-%m-%d', when, 'weekday 0') = '2024-01-21'`, want: 1},
		{name: "strftime literal", filter: `strftime('Q%m/%Y', when) = 'Q02/2024'`, want: 1},
		{name: "strftime percent", filter: `strftime('%%%Y', when) = '%2024'`, want: 3},
		{name: "strftime now", filter: `strftime('%Y') >= '2024'`, want: 8},
		{name: "strftime now value", filter: `strftime('%Y', 'now') >= '2024'`, want: 8},
		{name: "strftime text literal", filter: `strftime('%Y-%m', '2024-05-06 07:08:09') = '2024-05'`, want: 8},
		{name: "strftime literal date only", filter: `strftime('%d', '2024-05-06') = '06'`, want: 8},
		{name: "strftime param", filter: `strftime('%Y', {:d}) = '2022'`, params: dbx.Params{"d": "2022-11-05 10:20:30.000Z"}, want: 8},
		{name: "strftime empty date is null", filter: `strftime('%Y', when) = ''`, want: 3},
		{name: "strftime iso week", filter: `strftime('%V', when) = '03'`, want: 1},
		{name: "strftime week of year", filter: `strftime('%W', when) = '02'`, want: 0},
		{name: "strftime field vs field", filter: `strftime('%Y-%m', when) = strftime('%Y-%m', created)`, want: 0},
		{name: "strftime relation", collection: "comments", filter: `strftime('%Y', post.when) = '2024'`, want: 3},
		{name: "strftime multi relation", collection: "comments", filter: `strftime('%Y', seen.when) ?= '2024'`, want: 1},
		{name: "strftime unixepoch", filter: `strftime('%Y-%m-%d', 1705312800, 'unixepoch') = '2024-01-15'`, want: 8},
		{name: "strftime julian", filter: `strftime('%Y-%m-%d', 2460324.5) = '2024-01-15'`, want: 8},
		{name: "strftime unsupported format", filter: `strftime('%J', when) = '1'`, gap: "strftime formats %J and the century/locale ones are rejected on Postgres (SQLite %J returns a julian day float)", want: -1},
		{name: "strftime unsupported modifier", filter: `strftime('%Y', when, 'localtime') = '2024'`, gap: "the localtime and ceiling/floor strftime modifiers are rejected on Postgres", want: -1},
		{name: "strftime month overflow", filter: `strftime('%m-%d', '2024-01-31', '+1 month') = '03-02'`, gap: "SQLite normalizes month overflow forward (Jan 31 + 1 month = Mar 2), Postgres clamps to the last day (Feb 29)", want: -1},

		// ----- null / empty / boolean logic -----
		{name: "and or", filter: `(views > 5 && active = true) || title = "Zebra"`, want: 5},
		{name: "not nested", filter: `(views > 5 || views < 0) && !(active = true)`, wantErr: true, want: -1},
		{name: "or chain", filter: `views = 0 || views = 5.5 || views = -3`, want: 3},
		{name: "literal compare", filter: `1 = 1`, want: 8},
		{name: "literal compare text", filter: `"a" = "a"`, want: 8},
		{name: "literal compare false", filter: `"a" = "b"`, want: 0},
		{name: "literal like", filter: `"abc" ~ "b"`, want: 8},
		{name: "literal null", filter: `null = null`, want: 8},
		{name: "literal empty null", filter: `"" = null`, want: 8},
		{name: "literal true", filter: `true = true`, want: 8},
		{name: "literal true false", filter: `true = false`, want: 0},
		{name: "null compare", filter: `views != null && title != null`, want: 7},
		{name: "unknown field", filter: `nope = 1`, wantErr: true, want: -1},
		{name: "comment in filter", filter: "title = 'Zebra' // trailing comment", want: 1},
		{name: "param text", filter: `title = {:t}`, params: dbx.Params{"t": "Zebra"}, want: 1},
		{name: "param like", filter: `title ~ {:t}`, params: dbx.Params{"t": "hello"}, want: 3},
		{name: "param null", filter: `title = {:t}`, params: dbx.Params{"t": nil}, want: 1},
		{name: "param empty", filter: `title = {:t}`, params: dbx.Params{"t": ""}, want: 1},
		{name: "param quote", filter: `title = {:t}`, params: dbx.Params{"t": `a"b'c`}, want: 0},

		// ----- @request.* -----
		{name: "request method", filter: `@request.method = "GET"`, want: 0},
		{name: "request auth null", filter: `@request.auth.id = ""`, want: 8},
		{name: "request body null", filter: `@request.body.title = ""`, want: 8},
		{name: "request context", filter: `@request.context = "default"`, want: 0},
		{name: "request query empty", filter: `@request.query.x = ""`, want: 8},
		{name: "request headers empty", filter: `@request.headers.x = ""`, want: 8},
	})
}
