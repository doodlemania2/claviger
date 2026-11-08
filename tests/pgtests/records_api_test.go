//go:build postgres

package pgtests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dbutils"
	"github.com/doodlemania2/claviger/tools/types"
)

// apiClient sends requests to an app through its real router, so the whole
// records API (rules, filters, sort, expand, fields, batch) is exercised.
type apiClient struct {
	t       testing.TB
	handler http.Handler
	tokens  map[string]string
}

func newAPIClient(t testing.TB, app core.App) *apiClient {
	t.Helper()

	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	c := &apiClient{t: t, handler: handler, tokens: map[string]string{}}

	superusers, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	su := core.NewRecord(superusers)
	su.SetEmail("root@example.com")
	su.SetPassword("1234567890")
	if err := app.Save(su); err != nil {
		t.Fatal(err)
	}
	if c.tokens["super"], err = su.NewAuthToken(); err != nil {
		t.Fatal(err)
	}

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	u := core.NewRecord(users)
	u.Id = "usr0000000000u1"
	u.SetEmail("user@example.com")
	u.SetPassword("1234567890")
	u.SetVerified(true)
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	if c.tokens["user"], err = u.NewAuthToken(); err != nil {
		t.Fatal(err)
	}

	return c
}

type apiResult struct {
	Status int
	Body   any
}

func (c *apiClient) do(method, url, auth string, body any) apiResult {
	c.t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, url, reader)
	req.Header.Set("content-type", "application/json")
	if tok := c.tokens[auth]; tok != "" {
		req.Header.Set("Authorization", tok)
	}

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)

	res := apiResult{Status: rec.Code}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &res.Body); err != nil {
			c.t.Fatalf("%s %s: invalid response %q: %v", method, url, rec.Body.String(), err)
		}
	}

	return res
}

// normalize drops the values that legitimately differ between two runs.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			switch k {
			case "created", "updated", "token", "message":
				if _, isStr := val.(string); isStr {
					out[k] = "<volatile>"
					continue
				}
			}
			out[k] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalize(val)
		}
		return out
	}
	return v
}

type apiCase struct {
	name   string
	method string
	url    string
	auth   string // "", "super" or "user"
	body   any
	status int
	// unordered: compare the items of a list response as a set
	unordered bool
	// countOnly: the response is random, only the status is compared
	countOnly bool
	// check runs on the (normalized) Postgres response
	check func(t *testing.T, res apiResult)
	// gap documents a known difference: the case only runs on SQLite
	gap string
}

func runAPICases(t *testing.T, cases []apiCase, setup func(t testing.TB, app core.App)) {
	sqliteApp := newSQLiteApp(t)
	seedRecords(t, sqliteApp)
	pgApp := newApp(t)
	seedRecords(t, pgApp)
	if setup != nil {
		setup(t, sqliteApp)
		setup(t, pgApp)
	}

	sqliteClient := newAPIClient(t, sqliteApp)
	pgClient := newAPIClient(t, pgApp)

	for _, c := range cases {
		name := strings.NewReplacer(" ", "_", "/", "_").Replace(c.name)
		if c.gap != "" {
			name = "known_gap/" + name
		}

		t.Run(name, func(t *testing.T) {
			sqliteRes := sqliteClient.do(c.method, c.url, c.auth, c.body)

			if c.gap != "" {
				pgRes := pgClient.do(c.method, c.url, c.auth, c.body)
				t.Skipf("%s\n  request:  %s %s\n  sqlite:   %d %v\n  postgres: %d %v", c.gap, c.method, c.url, sqliteRes.Status, brief(sqliteRes.Body), pgRes.Status, brief(pgRes.Body))
			}

			pgRes := pgClient.do(c.method, c.url, c.auth, c.body)

			if sqliteRes.Status != c.status {
				t.Fatalf("sqlite status = %d, want %d: %s", sqliteRes.Status, c.status, brief(sqliteRes.Body))
			}
			if pgRes.Status != c.status {
				t.Fatalf("postgres status = %d, want %d: %s", pgRes.Status, c.status, brief(pgRes.Body))
			}

			want, got := normalize(sqliteRes.Body), normalize(pgRes.Body)
			if c.countOnly {
				want, got = nil, nil
			}
			if c.unordered {
				sortItems(want)
				sortItems(got)
			}

			if !reflect.DeepEqual(want, got) {
				t.Fatalf("response mismatch\n sqlite ids:   %v\n postgres ids: %v\n sqlite:   %s\n postgres: %s", itemIds(sqliteRes), itemIds(pgRes), brief(want), brief(got))
			}

			if c.check != nil {
				c.check(t, pgRes)
			}
		})
	}
}

func sortItems(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	items, ok := m["items"].([]any)
	if !ok {
		return
	}
	slices.SortFunc(items, func(a, b any) int {
		ia, _ := a.(map[string]any)["id"].(string)
		ib, _ := b.(map[string]any)["id"].(string)
		return strings.Compare(ia, ib)
	})
}

func brief(v any) string {
	raw, _ := json.Marshal(v)
	if len(raw) > 700 {
		return string(raw[:700]) + "..."
	}
	return string(raw)
}

func itemIds(res apiResult) []string {
	m, _ := res.Body.(map[string]any)
	items, _ := m["items"].([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		id, _ := it.(map[string]any)["id"].(string)
		ids = append(ids, strings.TrimPrefix(id, id[:min(3, len(id))]+"000000000"))
	}
	return ids
}

func expectIds(want ...string) func(*testing.T, apiResult) {
	return func(t *testing.T, res apiResult) {
		t.Helper()
		if got := itemIds(res); !slices.Equal(got, want) {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func expectTotal(want float64) func(*testing.T, apiResult) {
	return func(t *testing.T, res apiResult) {
		t.Helper()
		m, _ := res.Body.(map[string]any)
		if got, _ := m["totalItems"].(float64); got != want {
			t.Fatalf("totalItems = %v, want %v", m["totalItems"], want)
		}
	}
}

func TestRecordsList(t *testing.T) {
	list := func(name, query string, check func(*testing.T, apiResult)) apiCase {
		return apiCase{name: name, method: "GET", url: "/api/collections/posts/records?" + query, auth: "super", status: 200, check: check}
	}

	runAPICases(t, []apiCase{
		list("default", "sort=@rowid", expectIds("p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8")),
		list("default order", "", expectIds("p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8")),
		list("sort rowid desc", "sort=-@rowid", expectIds("p8", "p7", "p6", "p5", "p4", "p3", "p2", "p1")),
		list("sort number", "sort=views,id", expectIds("p6", "p3", "p4", "p7", "p1", "p8", "p2", "p5")),
		list("sort number desc", "sort=-views,-id", expectIds("p5", "p2", "p8", "p1", "p7", "p4", "p3", "p6")),
		list("sort text", "sort=title,id", expectIds("p5", "p4", "p1", "p8", "p3", "p6", "p2", "p7")),
		list("sort text desc", "sort=-title,id", expectIds("p7", "p2", "p6", "p3", "p1", "p8", "p4", "p5")),
		list("sort bool then rowid", "sort=active,@rowid", expectIds("p2", "p4", "p6", "p1", "p3", "p5", "p7", "p8")),
		list("sort nullable select asc", "sort=status,id", expectIds("p5", "p3", "p7", "p2", "p6", "p1", "p4", "p8")),
		list("sort nullable select desc", "sort=-status,id", expectIds("p1", "p4", "p8", "p2", "p6", "p3", "p7", "p5")),
		list("sort date", "sort=when,id", expectIds("p3", "p6", "p7", "p5", "p1", "p2", "p8", "p4")),
		list("sort json path", "sort=meta.n,id", nil),
		list("sort relation field", "sort=author.name,id", nil),
		list("sort relation field desc with filter", "sort=-author.rank,id&filter=views>0", nil),
		list("sort json path string", "sort=-meta.s,id", nil),
		{name: "sort json root", method: "GET", url: "/api/collections/posts/records?sort=meta,id", auth: "super", status: 200,
			gap: "JSON objects and arrays sort by their JSON text; Postgres renders it with spaces after the separators, SQLite compactly"},
		list("sort json geo", "sort=geo.lon,id", nil),
		{name: "sort multi relation field", method: "GET", url: "/api/collections/posts/records?sort=tags.name,id", auth: "super", status: 200,
			gap: "sorting by a field of a multiple relation: SQLite uses the first related record in array order, Postgres the smallest (ASC) or largest (DESC) value"},
		{name: "sort back relation", method: "GET", url: "/api/collections/posts/records?sort=comments_via_post.text,id", auth: "super", status: 200,
			gap: "sorting by a back relation field: SQLite uses the first scanned related record, Postgres the smallest (ASC) or largest (DESC) value"},
		list("sort multi relation field desc id only", "sort=-tags:length,id", nil),
		{name: "sort random with join", method: "GET", url: "/api/collections/posts/records?sort=@random&filter=" + url.QueryEscape(`tags.name ?~ "o"`), auth: "super", status: 200, countOnly: true, check: expectTotal(4)},
		{name: "sort random", method: "GET", url: "/api/collections/posts/records?sort=@random", auth: "super", status: 200, countOnly: true, check: func(t *testing.T, res apiResult) {
			if len(itemIds(res)) != 8 {
				t.Fatalf("expected 8 items, got %v", itemIds(res))
			}
		}},
		list("sort mixed", "sort=-active,views,-@rowid", nil),
		list("page", "sort=@rowid&page=2&perPage=3", expectIds("p4", "p5", "p6")),
		list("last page", "sort=@rowid&page=3&perPage=3", expectIds("p7", "p8")),
		list("page out of range", "sort=@rowid&page=9&perPage=3", expectIds()),
		list("total items", "perPage=1&filter=active=true", expectTotal(5)),
		list("total items with join", "perPage=1&filter=tags.name?='go'", expectTotal(4)),
		list("total items with multi join", "perPage=2&filter=tags:each?='tag000000000t2'", expectTotal(3)),
		list("skip total", "skipTotal=1&sort=@rowid&perPage=2", expectIds("p1", "p2")),
		list("filter and sort", "filter=views>5&sort=-views,id", expectIds("p5", "p2", "p1", "p8", "p7", "p4")),
		list("filter with join and distinct", "filter=tags.name?~'o'&sort=id", nil),
		list("filter back relation distinct sort", "filter=comments_via_seen.text?~'n'&sort=-views", nil),
		list("fields", "fields=id,title,views&sort=@rowid", nil),
		list("fields nested", "fields=id,expand.author.name&expand=author&sort=@rowid", nil),
		list("fields excerpt", "fields=id,body:excerpt(3,true)&sort=@rowid", nil),
		list("expand single", "expand=author&sort=@rowid", nil),
		list("expand multi", "expand=tags&sort=@rowid", nil),
		list("expand both", "expand=author,tags&sort=@rowid", nil),
		list("expand nested", "expand=author.posts_via_author&sort=@rowid", nil),
		list("expand back relation", "expand=comments_via_post&sort=@rowid", nil),
		list("expand back relation multi", "expand=comments_via_seen&sort=@rowid", nil),
		list("expand with filter", "expand=tags&filter=tags.name?='go'&sort=@rowid", nil),
		{name: "invalid filter", method: "GET", url: "/api/collections/posts/records?filter=nope=1", auth: "super", status: 400},
		{name: "invalid sort", method: "GET", url: "/api/collections/posts/records?sort=nope", auth: "super", status: 400},
		{name: "rule filter", method: "GET", url: "/api/collections/gated/records?sort=@rowid", auth: "user", status: 200},
		{name: "rule filter no auth", method: "GET", url: "/api/collections/gated/records?sort=@rowid", auth: "", status: 200},
		{name: "rule filter superuser", method: "GET", url: "/api/collections/gated/records?sort=@rowid", auth: "super", status: 200},
		{name: "rule with join filter", method: "GET", url: "/api/collections/gated/records?sort=@rowid&filter=friends.name?='go'", auth: "user", status: 200},
		{name: "request auth id", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@request.auth.id = "usr0000000000u1" && views > 10`), auth: "super", status: 200},
		{name: "request auth email", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@request.auth.email ~ "USER@" && active = true`), auth: "super", status: 200},
		{name: "request auth verified", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@request.auth.verified = true && active = false`), auth: "super", status: 200},
		{name: "request auth field", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@request.auth.created > "2000-01-01" && active = true`), auth: "super", status: 200},
		{name: "request query", method: "GET", url: "/api/collections/posts/records?sort=@rowid&q=Zebra&filter=" + url.QueryEscape(`title = @request.query.q`), auth: "super", status: 200},
		{name: "request query number", method: "GET", url: "/api/collections/posts/records?sort=@rowid&n=10&filter=" + url.QueryEscape(`views = @request.query.n`), auth: "super", status: 200},
		{name: "request query missing", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`title = @request.query.missing`), auth: "super", status: 200},
		{name: "request method", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@request.method = "GET" && views = 0`), auth: "super", status: 200},
		{name: "request context", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@request.context = "default" && views = 0`), auth: "super", status: 200},
		{name: "request query lower", method: "GET", url: "/api/collections/posts/records?sort=@rowid&q=ZEBRA&filter=" + url.QueryEscape(`title:lower = @request.query.q:lower`), auth: "super", status: 200},
		{name: "collection join", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@collection.authors.name ?= "Alice" && @collection.authors.rank > 0 && views > 50`), auth: "super", status: 200},
		{name: "collection join on field", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@collection.tags.name ?= title`), auth: "super", status: 200},
		{name: "collection join id", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@collection.authors.id ?= author`), auth: "super", status: 200},
		{name: "collection join alias", method: "GET", url: "/api/collections/posts/records?sort=@rowid&filter=" + url.QueryEscape(`@collection.tags:a.name ?= "go" && @collection.tags:b.name ?= "Rust"`), auth: "super", status: 200},
		{name: "collection join multi relation", method: "GET", url: "/api/collections/authors/records?sort=@rowid&filter=" + url.QueryEscape(`@collection.posts.tags.name ?= "go" && @collection.posts.author ?= id`), auth: "super", status: 200},
		{name: "list rule joins other collection rule", method: "GET", url: "/api/collections/gated/records?sort=@rowid&filter=" + url.QueryEscape(`post.title ?= "x" || friends.name ?= "go"`), auth: "user", status: 200},
		{name: "rule auth back relation", method: "GET", url: "/api/collections/ruled1/records?sort=@rowid", auth: "user", status: 200},
		{name: "rule auth multi relation", method: "GET", url: "/api/collections/ruled2/records?sort=@rowid", auth: "user", status: 200},
		{name: "rule auth fields", method: "GET", url: "/api/collections/ruled3/records?sort=@rowid", auth: "user", status: 200},
		{name: "rule auth fields no auth", method: "GET", url: "/api/collections/ruled3/records?sort=@rowid", auth: "", status: 200},
		{name: "rule collection join", method: "GET", url: "/api/collections/ruled4/records?sort=@rowid", auth: "user", status: 200},
		{name: "rule collection join no auth", method: "GET", url: "/api/collections/ruled4/records?sort=@rowid", auth: "", status: 200},
		{name: "rule relation to rule collection", method: "GET", url: "/api/collections/ruled5/records?sort=@rowid&expand=ref", auth: "user", status: 200},
		{name: "rule relation to rule collection filter", method: "GET", url: "/api/collections/ruled5/records?sort=@rowid&filter=" + url.QueryEscape(`ref.title ~ "a"`), auth: "user", status: 200},
		{name: "body rule ok", method: "POST", url: "/api/collections/ruled6/records", auth: "user", status: 200, body: map[string]any{"id": "ruled6000000001", "title": "ABC", "owner": "usr0000000000u1", "friends": []string{"tag000000000t1", "tag000000000t2"}}},
		{name: "body rule wrong title", method: "POST", url: "/api/collections/ruled6/records", auth: "user", status: 400, body: map[string]any{"title": "abd", "owner": "usr0000000000u1", "friends": []string{"tag000000000t1", "tag000000000t2"}}},
		{name: "body rule wrong length", method: "POST", url: "/api/collections/ruled6/records", auth: "user", status: 400, body: map[string]any{"title": "abc", "owner": "usr0000000000u1", "friends": []string{"tag000000000t1"}}},
		{name: "body rule wrong relation", method: "POST", url: "/api/collections/ruled6/records", auth: "user", status: 400, body: map[string]any{"title": "abc", "owner": "", "friends": []string{"tag000000000t1", "tag000000000t2"}}},
		{name: "view record", method: "GET", url: "/api/collections/posts/records/pos000000000p1?expand=author,tags", auth: "super", status: 200},
		{name: "view record missing", method: "GET", url: "/api/collections/posts/records/nope", auth: "super", status: 404},
		{name: "view record fields", method: "GET", url: "/api/collections/posts/records/pos000000000p1?fields=id,meta,geo,labels,files", auth: "super", status: 200},
		{name: "view record gated denied", method: "GET", url: "/api/collections/gated/records/gat000000000g2", auth: "", status: 404},
		{name: "view record gated owner", method: "GET", url: "/api/collections/gated/records/gat000000000g2", auth: "user", status: 200},
	}, seedGated)
}

func seedGated(t testing.TB, app core.App) {
	t.Helper()

	defer seedRuled(t, app)

	posts, err := app.FindCollectionByNameOrId("posts")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := app.FindCollectionByNameOrId("tags")
	if err != nil {
		t.Fatal(err)
	}

	gated := core.NewBaseCollection("gated")
	gated.Fields.Add(
		&core.TextField{Name: "title"},
		&core.NumberField{Name: "views"},
		&core.BoolField{Name: "published"},
		&core.RelationField{Name: "owner", CollectionId: "_pb_users_auth_", MaxSelect: 1},
		&core.RelationField{Name: "friends", CollectionId: tags.Id, MaxSelect: 5},
		&core.SelectField{Name: "kinds", Values: []string{"a", "b", "c"}, MaxSelect: 3},
		&core.RelationField{Name: "post", CollectionId: posts.Id, MaxSelect: 1},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	gated.ListRule = types.Pointer(`owner = @request.auth.id || published = true`)
	gated.ViewRule = types.Pointer(`owner = @request.auth.id || published = true`)
	gated.CreateRule = types.Pointer(`@request.auth.id != "" && @request.body.views > 5 && @request.body.kinds:length < 3 && @request.body.kinds:each ~ "a"`)
	gated.UpdateRule = types.Pointer(`owner = @request.auth.id && @request.body.title:isset = false && @request.body.views:changed = false`)
	gated.DeleteRule = types.Pointer(`owner = @request.auth.id && @request.auth.verified = true && @collection.tags.name ?= "Rust"`)
	if err := app.Save(gated); err != nil {
		t.Fatal(err)
	}

	for _, d := range []map[string]any{
		{"id": "gat000000000g1", "title": "pub", "views": 1, "published": true, "friends": []string{"tag000000000t1"}},
		{"id": "gat000000000g2", "title": "mine", "views": 2, "owner": "usr0000000000u1", "friends": []string{"tag000000000t1", "tag000000000t2"}},
		{"id": "gat000000000g3", "title": "private", "views": 3},
	} {
		r := core.NewRecord(gated)
		for k, v := range d {
			r.Set(k, v)
		}
		if err := app.SaveNoValidate(r); err != nil {
			t.Fatal(err)
		}
	}
}

// enableBatchAndNoCaseEmail turns the batch API on and makes the email unique
// index of the users collection case insensitive (COLLATE NOCASE).
func enableBatchAndNoCaseEmail(t testing.TB, app core.App) {
	t.Helper()

	settings := app.Settings()
	settings.Batch.Enabled = true
	if err := app.Save(settings); err != nil {
		t.Fatal(err)
	}

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range users.Indexes {
		if parsed := dbutils.ParseIndex(idx); strings.Contains(idx, "email") {
			users.RemoveIndex(parsed.IndexName)
		}
	}
	users.AddIndex("idx_users_email_nocase", true, "email COLLATE NOCASE", "email != ''")
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
}
func TestRecordsWrite(t *testing.T) {
	const posts = "/api/collections/posts/records"

	runAPICases(t, []apiCase{
		{name: "create all field types", method: "POST", url: posts, auth: "super", status: 200, body: map[string]any{
			"id": "pos0000000000p9", "title": "Created", "body": "<b>x</b>", "views": 42.25, "active": true,
			"status": "draft", "labels": []string{"a", "c"}, "author": "aut000000000a1", "tags": []string{"tag000000000t1", "tag000000000t3"},
			"meta": map[string]any{"n": 1, "list": []int{1, 2}}, "when": "2024-05-05 05:05:05.000Z",
			"site": "https://example.org", "contact": "c@example.org", "geo": map[string]any{"lon": 1.5, "lat": -2.5},
		}},
		{name: "created record is listed", method: "GET", url: posts + "/pos0000000000p9?expand=author,tags", auth: "super", status: 200},
		{name: "create with defaults", method: "POST", url: posts, auth: "super", status: 200, body: map[string]any{"id": "pos0000000000pa"}},
		{name: "create duplicate id", method: "POST", url: posts, auth: "super", status: 400, body: map[string]any{"id": "pos0000000000p9"}},
		{name: "create invalid select", method: "POST", url: posts, auth: "super", status: 400, body: map[string]any{"status": "nope"}},
		{name: "create invalid relation", method: "POST", url: posts, auth: "super", status: 400, body: map[string]any{"author": "doesnotexist000"}},
		{name: "create invalid email", method: "POST", url: posts, auth: "super", status: 400, body: map[string]any{"contact": "nope"}},
		{name: "update scalar fields", method: "PATCH", url: posts + "/pos0000000000p9", auth: "super", status: 200, body: map[string]any{
			"title": "Renamed", "views": 7, "active": false, "status": "published", "meta": map[string]any{"changed": true},
		}},
		{name: "update number modifiers", method: "PATCH", url: posts + "/pos0000000000p9", auth: "super", status: 200, body: map[string]any{"views+": 3, "labels-": "a", "tags+": "tag000000000t2"}},
		{name: "update modifiers prepend", method: "PATCH", url: posts + "/pos0000000000p9", auth: "super", status: 200, body: map[string]any{"+labels": "d", "tags-": []string{"tag000000000t3"}}},
		{name: "update clear relation", method: "PATCH", url: posts + "/pos0000000000p9", auth: "super", status: 200, body: map[string]any{"author": "", "tags": []string{}}},
		{name: "update missing", method: "PATCH", url: posts + "/nope", auth: "super", status: 404, body: map[string]any{"title": "x"}},
		{name: "view updated", method: "GET", url: posts + "/pos0000000000p9", auth: "super", status: 200},
		{name: "filter on created", method: "GET", url: posts + "?filter=views>8&sort=@rowid", auth: "super", status: 200},
		{name: "cascade: delete tag", method: "DELETE", url: "/api/collections/tags/records/tag000000000t1", auth: "super", status: 204},
		{name: "tag removed from multi relation", method: "GET", url: posts + "?sort=@rowid&fields=id,tags", auth: "super", status: 200},
		{name: "cascade: delete author", method: "DELETE", url: "/api/collections/authors/records/aut000000000a1", auth: "super", status: 204},
		{name: "author cleared from single relation", method: "GET", url: posts + "?sort=@rowid&fields=id,author", auth: "super", status: 200},
		{name: "delete", method: "DELETE", url: posts + "/pos0000000000pa", auth: "super", status: 204},
		{name: "deleted is gone", method: "GET", url: posts + "/pos0000000000pa", auth: "super", status: 404},
		{name: "delete missing", method: "DELETE", url: posts + "/pos0000000000pa", auth: "super", status: 404},
		{name: "count after writes", method: "GET", url: posts + "?perPage=1", auth: "super", status: 200},

		// batch
		{name: "batch", method: "POST", url: "/api/batch", auth: "super", status: 200, body: map[string]any{"requests": []map[string]any{
			{"method": "POST", "url": "/api/collections/posts/records", "body": map[string]any{"id": "pos0000000000pb", "title": "batched", "views": 1}},
			{"method": "PATCH", "url": "/api/collections/posts/records/pos000000000p1", "body": map[string]any{"title": "batched update"}},
			{"method": "PUT", "url": "/api/collections/posts/records", "body": map[string]any{"id": "pos0000000000pc", "title": "upserted"}},
			{"method": "PUT", "url": "/api/collections/posts/records", "body": map[string]any{"id": "pos0000000000pc", "title": "upserted again"}},
			{"method": "DELETE", "url": "/api/collections/posts/records/pos000000000p2"},
		}}},
		{name: "batch result", method: "GET", url: posts + "?sort=@rowid&fields=id,title", auth: "super", status: 200},
		{name: "batch rolls back", method: "POST", url: "/api/batch", auth: "super", status: 400, body: map[string]any{"requests": []map[string]any{
			{"method": "POST", "url": "/api/collections/posts/records", "body": map[string]any{"id": "pos0000000000pd", "title": "must not exist"}},
			{"method": "POST", "url": "/api/collections/posts/records", "body": map[string]any{"id": "pos0000000000pe", "status": "invalid"}},
		}}},
		{name: "batch rollback result", method: "GET", url: posts + "/pos0000000000pd", auth: "super", status: 404},

		// rules that use @request.*
		{name: "gated create", method: "POST", url: "/api/collections/gated/records", auth: "user", status: 200, body: map[string]any{"id": "gat0000000000g4", "title": "ok", "views": 10, "kinds": []string{"a"}, "owner": "usr0000000000u1"}},
		{name: "gated create low views", method: "POST", url: "/api/collections/gated/records", auth: "user", status: 400, body: map[string]any{"title": "no", "views": 1, "kinds": []string{"a"}}},
		{name: "gated create too many kinds", method: "POST", url: "/api/collections/gated/records", auth: "user", status: 400, body: map[string]any{"title": "no", "views": 10, "kinds": []string{"a", "b", "c"}}},
		{name: "gated create kind mismatch", method: "POST", url: "/api/collections/gated/records", auth: "user", status: 400, body: map[string]any{"title": "no", "views": 10, "kinds": []string{"b"}}},
		{name: "gated create no auth", method: "POST", url: "/api/collections/gated/records", auth: "", status: 400, body: map[string]any{"views": 10, "kinds": []string{"a"}}},
		{name: "gated update", method: "PATCH", url: "/api/collections/gated/records/gat0000000000g4", auth: "user", status: 200, body: map[string]any{"published": true}},
		{name: "gated update with title", method: "PATCH", url: "/api/collections/gated/records/gat0000000000g4", auth: "user", status: 404, body: map[string]any{"title": "x"}},
		{name: "gated update views changed", method: "PATCH", url: "/api/collections/gated/records/gat0000000000g4", auth: "user", status: 404, body: map[string]any{"published": false, "views": 99}},
		{name: "gated update views same", method: "PATCH", url: "/api/collections/gated/records/gat0000000000g4", auth: "user", status: 200, body: map[string]any{"views": 10}},
		{name: "gated delete", method: "DELETE", url: "/api/collections/gated/records/gat0000000000g4", auth: "user", status: 204},

		// auth, case insensitive identity (COLLATE NOCASE on SQLite)
		{name: "auth with password", method: "POST", url: "/api/collections/users/auth-with-password", status: 200, body: map[string]any{"identity": "user@example.com", "password": "1234567890"}},
		{name: "auth with password other case", method: "POST", url: "/api/collections/users/auth-with-password", status: 200, body: map[string]any{"identity": "USER@Example.com", "password": "1234567890"}},
		{name: "auth with wrong password", method: "POST", url: "/api/collections/users/auth-with-password", status: 400, body: map[string]any{"identity": "user@example.com", "password": "nope"}},
		{name: "users list", method: "GET", url: "/api/collections/users/records?filter=email~'USER'", auth: "super", status: 200},
		{name: "users list by verified", method: "GET", url: "/api/collections/users/records?filter=verified=true", auth: "super", status: 200},
		{name: "user email visibility filter", method: "GET", url: "/api/collections/users/records?filter=email='user@example.com'", auth: "user", status: 200},
	}, func(t testing.TB, app core.App) {
		seedGated(t, app)
		enableBatchAndNoCaseEmail(t, app)
	})
}

// seedRuled adds collections whose list rule uses auth relations, back
// relations and @collection joins (the request rules).
func seedRuled(t testing.TB, app core.App) {
	t.Helper()

	gated, err := app.FindCollectionByNameOrId("gated")
	if err != nil {
		t.Fatal(err)
	}

	rules := map[string]string{
		"ruled1": `@request.auth.gated_via_owner.title ?= "mine"`,
		"ruled2": `@request.auth.gated_via_owner.friends.name ?= "Rust"`,
		"ruled3": `@request.auth.email ~ "USER@" && @request.auth.verified = true && @request.auth.created > "2000-01-01"`,
		"ruled4": `@request.auth.id = owner || @collection.tags.name ?= title`,
	}

	for name, rule := range rules {
		c := core.NewBaseCollection(name)
		c.Fields.Add(
			&core.TextField{Name: "title"},
			&core.RelationField{Name: "owner", CollectionId: "_pb_users_auth_", MaxSelect: 1},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.ListRule = types.Pointer(rule)
		if err := app.Save(c); err != nil {
			t.Fatal(err)
		}

		for i, title := range []string{"go", "alpha", "Rust"} {
			r := core.NewRecord(c)
			r.Id = name + "0000000000" + string(rune('a'+i))
			r.Set("title", title)
			if i == 0 {
				r.Set("owner", "usr0000000000u1")
			}
			if err := app.SaveNoValidate(r); err != nil {
				t.Fatal(err)
			}
		}
	}

	// create rule over the request body: relation paths, :lower, :each, :length
	body := core.NewBaseCollection("ruled6")
	body.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "owner", CollectionId: "_pb_users_auth_", MaxSelect: 1},
		&core.RelationField{Name: "friends", CollectionId: gated.Fields.GetByName("friends").(*core.RelationField).CollectionId, MaxSelect: 5},
		&core.AutodateField{Name: "created", OnCreate: true},
	)
	body.CreateRule = types.Pointer(`@request.body.owner.email = "user@example.com" && @request.body.title:lower = "abc" && ` +
		`@request.body.friends.name ?= "go" && @request.body.friends:each ?= "tag000000000t1" && @request.body.friends:length = 2`)
	if err := app.Save(body); err != nil {
		t.Fatal(err)
	}

	// a relation to a collection that has a list rule
	ref := core.NewBaseCollection("ruled5")
	ref.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "ref", CollectionId: gated.Id, MaxSelect: 1},
	)
	ref.ListRule = types.Pointer("")
	if err := app.Save(ref); err != nil {
		t.Fatal(err)
	}
	for i, target := range []string{"gat000000000g1", "gat000000000g2", "gat000000000g3"} {
		r := core.NewRecord(ref)
		r.Id = "ruled50000000" + string(rune('a'+i)) + "0"
		r.Set("title", "ref"+string(rune('a'+i)))
		r.Set("ref", target)
		if err := app.SaveNoValidate(r); err != nil {
			t.Fatal(err)
		}
	}
}
