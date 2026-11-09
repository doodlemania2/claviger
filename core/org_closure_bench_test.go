package core_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #84, milestone 84-2): the cost of the closure join.
//
//	go test ./core/ -run xxx -bench OrgClosureList -benchtime 20x
//
// Shape (the #79 benchmark): about 500 orgs in four levels, 40 users with a
// membership in every org (20,000 memberships) and 200,000 records in an
// org-owned collection indexed on `org`. CLAVIGER_BENCH_RECORDS overrides the
// record count (setup of 200k rows takes a while on Postgres). On Postgres:
// cd scripts/pgembedded && CLAVIGER_TEST_DB=postgres go run . -run xxx -bench OrgClosureList ./core/
func BenchmarkOrgClosureList(b *testing.B) {
	records := 200000
	if v, err := strconv.Atoi(os.Getenv("CLAVIGER_BENCH_RECORDS")); err == nil && v > 0 {
		records = v
	}

	f := newClosureFixture(b)

	// 1 root, 4 at level 2, 16 at level 3, 479 at level 4: 500 orgs
	var ids []string
	root := f.org(b, "b-0", "")
	ids = append(ids, root.Id)

	level3 := []string{}
	n := 1
	for i := 0; i < 4; i++ {
		l2 := f.org(b, fmt.Sprintf("b-%d", n), root.Id)
		n++
		ids = append(ids, l2.Id)
		for j := 0; j < 4; j++ {
			l3 := f.org(b, fmt.Sprintf("b-%d", n), l2.Id)
			n++
			ids = append(ids, l3.Id)
			level3 = append(level3, l3.Id)
		}
	}

	leaf := ""
	for n < 500 {
		o := f.org(b, fmt.Sprintf("b-%d", n), level3[n%len(level3)])
		leaf = o.Id
		ids = append(ids, o.Id)
		n++
	}

	usersCol, _ := f.app.FindCollectionByNameOrId("users")
	membersCol, _ := f.app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	userRecs := []*core.Record{}

	for i := 0; i < 40; i++ {
		u := core.NewRecord(usersCol)
		u.SetEmail(fmt.Sprintf("bench%d@closure.test", i))
		u.SetPassword("password123")
		if err := f.app.Save(u); err != nil {
			b.Fatal(err)
		}
		userRecs = append(userRecs, u)
	}

	raw := f.app.UnsafeWithoutHooks()
	for _, o := range ids {
		for i, u := range userRecs {
			m := core.NewRecord(membersCol)
			m.Id = core.GenerateDefaultRandomId()
			m.Set("org", o)
			m.Set("user", u.Id)
			m.Set("status", "active")
			m.Set("role", "member")
			// user 0 is an admin of the root, user 1 only of one level 4 org
			if i == 0 && o == root.Id {
				m.Set("role", "admin")
			}
			if err := raw.SaveNoValidate(m); err != nil {
				b.Fatal(err)
			}
		}
	}

	col := core.NewBaseCollection("bench_docs")
	col.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "org", CollectionId: f.orgs.Id, MaxSelect: 1, Required: true},
	)
	col.AddIndex("idx_bench_docs_org", false, "org", "")
	if err := f.app.Save(col); err != nil {
		b.Fatal(err)
	}

	db := f.app.NonconcurrentDB()
	const batch = 400

	for from := 0; from < records; from += batch {
		to := min(from+batch, records)
		params := dbx.Params{}

		var sb strings.Builder
		sb.WriteString("INSERT INTO {{bench_docs}} ([[id]], [[title]], [[org]]) VALUES ")

		for i := from; i < to; i++ {
			if i > from {
				sb.WriteString(",")
			}

			fmt.Fprintf(&sb, "({:i%d},{:t%d},{:o%d})", i, i, i)
			params[fmt.Sprintf("i%d", i)] = fmt.Sprintf("doc%012d", i)
			params[fmt.Sprintf("t%d", i)] = "t"
			params[fmt.Sprintf("o%d", i)] = ids[i%len(ids)]
		}

		if _, err := db.NewQuery(sb.String()).Bind(params).Execute(); err != nil {
			b.Fatal(err)
		}
	}

	ancestor, _ := core.OrgAncestorRule("org", core.OrgAncestorOpts{})
	ancestorAdmin, _ := core.OrgAncestorRule("org", core.OrgAncestorOpts{Roles: []string{"admin"}, MaxDepth: 3})
	orgOnly := core.OrgMemberRule("org")

	admin := userRecs[0] // sees all 500 orgs through the oversight rule
	plain := userRecs[2] // member of every org, no admin role

	cases := []struct {
		name  string
		user  *core.Record
		rule  string
		query string
	}{
		{"orgOnly/unfiltered_with_total", plain, orgOnly, "perPage=30"},
		{"ancestor/unfiltered_with_total", plain, ancestor, "perPage=30"},
		{"ancestor/unfiltered_skipTotal", plain, ancestor, "perPage=30&skipTotal=1"},
		{"ancestor/filter_org_skipTotal", plain, ancestor, "perPage=30&skipTotal=1&filter=" + "org%3D%22" + leaf + "%22"},
		{"ancestorAdminDepth3/filter_org_skipTotal", admin, ancestorAdmin, "perPage=30&skipTotal=1&filter=" + "org%3D%22" + leaf + "%22"},
		{"ancestorAdminDepth3/unfiltered_skipTotal", admin, ancestorAdmin, "perPage=30&skipTotal=1"},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				listWithRule(b, f.app, col, c.user, c.rule, c.query)
			}
		})
	}
}
