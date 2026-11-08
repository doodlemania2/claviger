package search_test

import (
	"regexp"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/search"
)

// TestSQLiteFilterGolden pins the exact SQL that the filter language produced
// on SQLite before the dialect layer of milestone 2 (#106). These strings were
// captured from master and must never change: production runs SQLite.
func TestSQLiteFilterGolden(t *testing.T) {
	resolver := search.NewSimpleFieldResolver("a", "b", "c", "g1", "g2", `^j\.\w+$`)
	db := dbx.NewFromDB(nil, "sqlite")
	placeholders := regexp.MustCompile(`\{:[a-zA-Z0-9]+\}`)

	scenarios := []struct{ filter, sql string }{
		{"a = 1", "[[a]] = {:P}"},
		{"a != 1", "[[a]] IS NOT {:P}"},
		{"a = 'x'", "[[a]] = {:P}"},
		{"a != 'x'", "[[a]] IS NOT {:P}"},
		{"a = true", "[[a]] = 1"},
		{"a != true", "[[a]] IS NOT 1"},
		{"a = false", "[[a]] = 0"},
		{"b = null", "(([[b]] = '' OR [[b]] IS NULL))"},
		{"b != null", "(([[b]] IS NOT '' AND [[b]] IS NOT NULL))"},
		{"a = b", "COALESCE([[a]], '') = COALESCE([[b]], '')"},
		{"a != b", "COALESCE([[a]], '') IS NOT COALESCE([[b]], '')"},
		{"a ~ 'x'", "[[a]] LIKE {:P} ESCAPE '\\'"},
		{"a !~ 'x'", "[[a]] NOT LIKE {:P} ESCAPE '\\'"},
		{"a ~ 'x%'", "[[a]] LIKE {:P} ESCAPE '\\'"},
		{"a ~ b", "[[a]] LIKE ('%' || [[b]] || '%') ESCAPE '\\'"},
		{"'lorem' ~ a", "{:P} LIKE ('%' || [[a]] || '%') ESCAPE '\\'"},
		{"a > 1 && a >= 2 && a < 3 && a <= 4", "([[a]] > {:P} AND [[a]] >= {:P} AND [[a]] < {:P} AND [[a]] <= {:P})"},
		{"a ?= 'x'", "[[a]] = {:P}"},
		{"a ?!= 'x'", "[[a]] IS NOT {:P}"},
		{"a ?~ 'x'", "[[a]] LIKE {:P} ESCAPE '\\'"},
		{"a ?!~ 'x'", "[[a]] NOT LIKE {:P} ESCAPE '\\'"},
		{"a ?> 1", "[[a]] > {:P}"},
		{"a ?< 1", "[[a]] < {:P}"},
		{"a ?>= 1", "[[a]] >= {:P}"},
		{"a ?<= 1", "[[a]] <= {:P}"},
		{"j.x = 1", "JSON_EXTRACT([[j]], '$.x') IS {:P}"},
		{"j.x != j.y", "JSON_EXTRACT([[j]], '$.x') IS NOT JSON_EXTRACT([[j]], '$.y')"},
		{"j.x ~ 'q'", "JSON_EXTRACT([[j]], '$.x') LIKE {:P} ESCAPE '\\'"},
		{"j.x = null", "JSON_EXTRACT([[j]], '$.x') IS NULL"},
		{"j.x > 1 || j.y = true", "(JSON_EXTRACT([[j]], '$.x') > {:P} OR JSON_EXTRACT([[j]], '$.y') IS 1)"},
		{"b = ''", "(([[b]] = '' OR [[b]] IS NULL))"},
		{"b != ''", "(([[b]] IS NOT '' AND [[b]] IS NOT NULL))"},
		{"strftime('%Y-%m', c) = '2024-01'", "strftime({:P},[[c]]) = {:P}"},
		{"strftime('%Y-%m-%d %H:%M:%f', c, 'start of month', '+1 day') > c", "strftime({:P},[[c]],{:P},{:P}) > [[c]]"},
		{"strftime('%Y') = '2024'", "strftime({:P}) = {:P}"},
		{"strftime('%s', 'now', '-1 day') > 5", "strftime({:P},{:P},{:P}) > {:P}"},
		{"geoDistance(1, 2, 3, 4) < 10", "(6371 * acos(min(1, max(-1, cos(radians({:P})) * cos(radians({:P})) * cos(radians({:P}) - radians({:P})) + sin(radians({:P})) * sin(radians({:P})))))) < {:P}"},
		{"geoDistance(g1, g2, 3, 4) < 10", "(6371 * acos(min(1, max(-1, cos(radians([[g2]])) * cos(radians({:P})) * cos(radians({:P}) - radians([[g1]])) + sin(radians([[g2]])) * sin(radians({:P})))))) < {:P}"},
		{"a = @now || c > @todayStart", "([[a]] = {:P} OR [[c]] > {:P})"},
	}

	for _, s := range scenarios {
		t.Run(s.filter, func(t *testing.T) {
			expr, err := search.FilterData(s.filter).BuildExpr(resolver)
			if err != nil {
				t.Fatal(err)
			}

			got := placeholders.ReplaceAllString(expr.Build(db, dbx.Params{}), "{:P}")
			if got != s.sql {
				t.Fatalf("SQLite SQL changed\nwant: %s\ngot:  %s", s.sql, got)
			}
		})
	}
}

func TestSQLiteSortGolden(t *testing.T) {
	resolver := search.NewSimpleFieldResolver("a", "b")

	for sortStr, want := range map[string]string{
		"-a":      "[[a]] DESC",
		"+b":      "[[b]] ASC",
		"@random": "RANDOM()",
		"-@rowid": "[[_rowid_]] DESC",
	} {
		f := search.ParseSortFromString(sortStr)[0]
		got, err := f.BuildExpr(resolver)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: want %q, got %q", sortStr, want, got)
		}
	}
}
