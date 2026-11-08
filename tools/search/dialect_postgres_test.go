package search_test

import (
	"regexp"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
	"github.com/doodlemania2/claviger/tools/search"
)

// pgResolver is a field resolver for the Postgres dialect: "j.<key>" is a
// JSON path of the jsonb column "j", the other names are plain columns.
type pgResolver struct{ *search.SimpleFieldResolver }

func (pgResolver) Dialect() dialect.Dialect { return dialect.Postgres }

func (r pgResolver) Resolve(field string) (*search.ResolverResult, error) {
	if key, ok := strings.CutPrefix(field, "j."); ok {
		return &search.ResolverResult{
			NullFallback: search.NullFallbackDisabled,
			JSONB:        true,
			Identifier:   dialect.Postgres.JSONExtract("j", key),
		}, nil
	}

	return r.SimpleFieldResolver.Resolve(field)
}

func TestPostgresFilterSQL(t *testing.T) {
	resolver := pgResolver{search.NewSimpleFieldResolver("a", "b", "c", "g1", "g2", `^j\.\w+$`)}
	db := dbx.NewFromDB(nil, "pgx")
	placeholders := regexp.MustCompile(`\{:[a-zA-Z0-9]+\}`)

	scenarios := []struct{ filter, sql string }{
		{"a = 1", "[[a]] = {:P}"},
		{"a != 1", "[[a]] IS DISTINCT FROM {:P}"},
		{"a = 'x'", "[[a]] = {:P}"},
		{"a != 'x'", "[[a]] IS DISTINCT FROM {:P}"},
		{"a = true", "[[a]] = TRUE"},
		{"a != true", "[[a]] IS DISTINCT FROM TRUE"},
		{"a = false", "[[a]] = FALSE"},
		{"b = null", "((CAST([[b]] AS TEXT) = '' OR [[b]] IS NULL))"},
		{"b != null", "((CAST([[b]] AS TEXT) IS DISTINCT FROM '' AND [[b]] IS NOT NULL))"},
		{"a = b", "COALESCE(CAST([[a]] AS TEXT), '') = COALESCE(CAST([[b]] AS TEXT), '')"},
		{"a != b", "COALESCE(CAST([[a]] AS TEXT), '') IS DISTINCT FROM COALESCE(CAST([[b]] AS TEXT), '')"},
		{"a ~ 'x'", "CAST([[a]] AS TEXT) ILIKE {:P} ESCAPE '\\'"},
		{"a !~ 'x'", "CAST([[a]] AS TEXT) NOT ILIKE {:P} ESCAPE '\\'"},
		{"a ~ 'x%'", "CAST([[a]] AS TEXT) ILIKE {:P} ESCAPE '\\'"},
		{"a ~ b", "CAST([[a]] AS TEXT) ILIKE ('%' || CAST([[b]] AS TEXT) || '%') ESCAPE '\\'"},
		{"'lorem' ~ a", "CAST({:P} AS TEXT) ILIKE ('%' || CAST([[a]] AS TEXT) || '%') ESCAPE '\\'"},
		{"a > 1 && a >= 2 && a < 3 && a <= 4", "([[a]] > {:P} AND [[a]] >= {:P} AND [[a]] < {:P} AND [[a]] <= {:P})"},
		{"a ?= 'x'", "[[a]] = {:P}"},
		{"a ?!= 'x'", "[[a]] IS DISTINCT FROM {:P}"},
		{"a ?~ 'x'", "CAST([[a]] AS TEXT) ILIKE {:P} ESCAPE '\\'"},
		{"a ?!~ 'x'", "CAST([[a]] AS TEXT) NOT ILIKE {:P} ESCAPE '\\'"},
		{"a ?> 1", "[[a]] > {:P}"},
		{"a ?< 1", "[[a]] < {:P}"},
		{"a ?>= 1", "[[a]] >= {:P}"},
		{"a ?<= 1", "[[a]] <= {:P}"},
		{"j.x = 1", "NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) IS NOT DISTINCT FROM to_jsonb(CAST({:P} AS DOUBLE PRECISION))"},
		{"j.x != j.y", "NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) IS DISTINCT FROM NULLIF([[j]] #> '{\"y\"}', 'null'::jsonb)"},
		{"j.x ~ 'q'", "CAST((NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) #>> '{}') AS TEXT) ILIKE {:P} ESCAPE '\\'"},
		{"j.x = null", "NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) IS NOT DISTINCT FROM NULL"},
		{"j.x > 1 || j.y = true", "(NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) > to_jsonb(CAST({:P} AS DOUBLE PRECISION)) OR NULLIF([[j]] #> '{\"y\"}', 'null'::jsonb) IS NOT DISTINCT FROM to_jsonb(CAST(TRUE AS BOOLEAN)))"},
		{"b = ''", "((CAST([[b]] AS TEXT) = '' OR [[b]] IS NULL))"},
		{"b != ''", "((CAST([[b]] AS TEXT) IS DISTINCT FROM '' AND [[b]] IS NOT NULL))"},
		{"strftime('%Y-%m', c) = '2024-01'", "((SELECT to_char(pb_ts.ts, 'YYYY\"-\"MM') FROM (SELECT CASE WHEN lower(pb_in.t) = 'now' THEN NOW() AT TIME ZONE 'UTC' WHEN pb_in.t ~ ':[0-9]{2}(\\.[0-9]+)?\\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$' AND pg_input_is_valid(pb_in.t, 'timestamptz') THEN CAST(pb_in.t AS TIMESTAMPTZ) AT TIME ZONE 'UTC' WHEN pg_input_is_valid(pb_in.t, 'timestamp') THEN CAST(pb_in.t AS TIMESTAMP) END AS ts FROM (SELECT CAST([[c]] AS TEXT) AS t) AS pb_in) AS pb_ts) = {:P})"},
		{"strftime('%Y-%m-%d %H:%M:%f', c, 'start of month', '+1 day') > c", "((SELECT to_char(pb_ts.ts, 'YYYY\"-\"MM\"-\"DD\" \"HH24\":\"MI\":\"SS.MS') FROM (SELECT (date_trunc('month', CASE WHEN lower(pb_in.t) = 'now' THEN NOW() AT TIME ZONE 'UTC' WHEN pb_in.t ~ ':[0-9]{2}(\\.[0-9]+)?\\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$' AND pg_input_is_valid(pb_in.t, 'timestamptz') THEN CAST(pb_in.t AS TIMESTAMPTZ) AT TIME ZONE 'UTC' WHEN pg_input_is_valid(pb_in.t, 'timestamp') THEN CAST(pb_in.t AS TIMESTAMP) END)) + INTERVAL '+1 day' AS ts FROM (SELECT CAST([[c]] AS TEXT) AS t) AS pb_in) AS pb_ts) > [[c]])"},
		{"strftime('%Y') = '2024'", "(SELECT to_char(pb_ts.ts, 'YYYY') FROM (SELECT NOW() AT TIME ZONE 'UTC' AS ts FROM (SELECT 1 AS t) AS pb_in) AS pb_ts) = {:P}"},
		{"geoDistance(1, 2, 3, 4) < 10", "(6371 * acos(LEAST(1, GREATEST(-1, cos(radians(CAST({:P} AS DOUBLE PRECISION))) * cos(radians(CAST({:P} AS DOUBLE PRECISION))) * cos(radians(CAST({:P} AS DOUBLE PRECISION)) - radians(CAST({:P} AS DOUBLE PRECISION))) + sin(radians(CAST({:P} AS DOUBLE PRECISION))) * sin(radians(CAST({:P} AS DOUBLE PRECISION))))))) < {:P}"},
		{"geoDistance(g1, g2, 3, 4) < 10", "(6371 * acos(LEAST(1, GREATEST(-1, cos(radians(CAST([[g2]] AS DOUBLE PRECISION))) * cos(radians(CAST({:P} AS DOUBLE PRECISION))) * cos(radians(CAST({:P} AS DOUBLE PRECISION)) - radians(CAST([[g1]] AS DOUBLE PRECISION))) + sin(radians(CAST([[g2]] AS DOUBLE PRECISION))) * sin(radians(CAST({:P} AS DOUBLE PRECISION))))))) < {:P}"},
		{"a = @now || c > @todayStart", "([[a]] = {:P} OR [[c]] > {:P})"},
		{"j.x = 'a' || j.x = 1", "(NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) IS NOT DISTINCT FROM to_jsonb(CAST({:P} AS TEXT)) OR NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) IS NOT DISTINCT FROM to_jsonb(CAST({:P} AS DOUBLE PRECISION)))"},
		{"j.x = a", "(NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) #>> '{}') IS NOT DISTINCT FROM [[a]]"},
		{"j.x ~ a", "CAST((NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) #>> '{}') AS TEXT) ILIKE ('%' || CAST([[a]] AS TEXT) || '%') ESCAPE '\\'"},
		{"j.x = j.y", "NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) IS NOT DISTINCT FROM NULLIF([[j]] #> '{\"y\"}', 'null'::jsonb)"},
		{"j.x != null", "NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) IS DISTINCT FROM NULL"},
		{"a = j.x", "[[a]] IS NOT DISTINCT FROM (NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) #>> '{}')"},
		{"strftime('%Y-%m-%d %H:%M', c, '+1 day', 'start of month') = '2024-01-01 00:00'", "((SELECT to_char(pb_ts.ts, 'YYYY\"-\"MM\"-\"DD\" \"HH24\":\"MI') FROM (SELECT date_trunc('month', (CASE WHEN lower(pb_in.t) = 'now' THEN NOW() AT TIME ZONE 'UTC' WHEN pb_in.t ~ ':[0-9]{2}(\\.[0-9]+)?\\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$' AND pg_input_is_valid(pb_in.t, 'timestamptz') THEN CAST(pb_in.t AS TIMESTAMPTZ) AT TIME ZONE 'UTC' WHEN pg_input_is_valid(pb_in.t, 'timestamp') THEN CAST(pb_in.t AS TIMESTAMP) END) + INTERVAL '+1 day') AS ts FROM (SELECT CAST([[c]] AS TEXT) AS t) AS pb_in) AS pb_ts) = {:P})"},
		{"strftime('%s', 2460324.5) > 5", "(SELECT CAST(CAST(floor(EXTRACT(EPOCH FROM (pb_ts.ts AT TIME ZONE 'UTC'))) AS BIGINT) AS TEXT) FROM (SELECT to_timestamp((pb_in.t - 2440587.5) * 86400.0) AT TIME ZONE 'UTC' AS ts FROM (SELECT CAST({:P} AS DOUBLE PRECISION) AS t) AS pb_in) AS pb_ts) > {:P}"},
		{"strftime('%Y', j.x) = '2024'", "((SELECT to_char(pb_ts.ts, 'YYYY') FROM (SELECT CASE WHEN lower(pb_in.t) = 'now' THEN NOW() AT TIME ZONE 'UTC' WHEN pb_in.t ~ ':[0-9]{2}(\\.[0-9]+)?\\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$' AND pg_input_is_valid(pb_in.t, 'timestamptz') THEN CAST(pb_in.t AS TIMESTAMPTZ) AT TIME ZONE 'UTC' WHEN pg_input_is_valid(pb_in.t, 'timestamp') THEN CAST(pb_in.t AS TIMESTAMP) END AS ts FROM (SELECT CAST((NULLIF([[j]] #> '{\"x\"}', 'null'::jsonb) #>> '{}') AS TEXT) AS t) AS pb_in) AS pb_ts) = {:P})"},
		{"strftime('%Y-%m-%d', c, 'start of month', '+1 month', '-1 day') = '2024-01-31'", "((SELECT to_char(pb_ts.ts, 'YYYY\"-\"MM\"-\"DD') FROM (SELECT ((date_trunc('month', CASE WHEN lower(pb_in.t) = 'now' THEN NOW() AT TIME ZONE 'UTC' WHEN pb_in.t ~ ':[0-9]{2}(\\.[0-9]+)?\\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$' AND pg_input_is_valid(pb_in.t, 'timestamptz') THEN CAST(pb_in.t AS TIMESTAMPTZ) AT TIME ZONE 'UTC' WHEN pg_input_is_valid(pb_in.t, 'timestamp') THEN CAST(pb_in.t AS TIMESTAMP) END)) + INTERVAL '+1 month') + INTERVAL '-1 day' AS ts FROM (SELECT CAST([[c]] AS TEXT) AS t) AS pb_in) AS pb_ts) = {:P})"},
		{"strftime('Q%m/%Y week %W day %w', c) = 'x'", "((SELECT to_char(pb_ts.ts, '\"Q\"MM\"/\"YYYY\" week \"') || lpad(CAST(((CAST(EXTRACT(DOY FROM pb_ts.ts) AS INTEGER) - 1 + 7 - ((CAST(EXTRACT(DOW FROM pb_ts.ts) AS INTEGER) + 6) % 7)) / 7) AS TEXT), 2, '0') || to_char(pb_ts.ts, '\" day \"') || CAST(CAST(EXTRACT(DOW FROM pb_ts.ts) AS INTEGER) AS TEXT) FROM (SELECT CASE WHEN lower(pb_in.t) = 'now' THEN NOW() AT TIME ZONE 'UTC' WHEN pb_in.t ~ ':[0-9]{2}(\\.[0-9]+)?\\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$' AND pg_input_is_valid(pb_in.t, 'timestamptz') THEN CAST(pb_in.t AS TIMESTAMPTZ) AT TIME ZONE 'UTC' WHEN pg_input_is_valid(pb_in.t, 'timestamp') THEN CAST(pb_in.t AS TIMESTAMP) END AS ts FROM (SELECT CAST([[c]] AS TEXT) AS t) AS pb_in) AS pb_ts) = {:P})"},
		{"strftime('%s', 'now', '-1 day') > 5", "((SELECT CAST(CAST(floor(EXTRACT(EPOCH FROM (pb_ts.ts AT TIME ZONE 'UTC'))) AS BIGINT) AS TEXT) FROM (SELECT (CASE WHEN lower(pb_in.t) = 'now' THEN NOW() AT TIME ZONE 'UTC' WHEN pb_in.t ~ ':[0-9]{2}(\\.[0-9]+)?\\s*(Z|z|[+-][0-9]{2}(:?[0-9]{2})?)$' AND pg_input_is_valid(pb_in.t, 'timestamptz') THEN CAST(pb_in.t AS TIMESTAMPTZ) AT TIME ZONE 'UTC' WHEN pg_input_is_valid(pb_in.t, 'timestamp') THEN CAST(pb_in.t AS TIMESTAMP) END) + INTERVAL '-1 day' AS ts FROM (SELECT CAST({:P} AS TEXT) AS t) AS pb_in) AS pb_ts) > {:P})"},
		{"j.n >= 1 && j.n <= 2", "(NULLIF([[j]] #> '{\"n\"}', 'null'::jsonb) >= to_jsonb(CAST({:P} AS DOUBLE PRECISION)) AND NULLIF([[j]] #> '{\"n\"}', 'null'::jsonb) <= to_jsonb(CAST({:P} AS DOUBLE PRECISION)))"},
	}

	for _, s := range scenarios {
		t.Run(s.filter, func(t *testing.T) {
			expr, err := search.FilterData(s.filter).BuildExpr(resolver)
			if err != nil {
				t.Fatal(err)
			}

			got := placeholders.ReplaceAllString(expr.Build(db, dbx.Params{}), "{:P}")
			if got != s.sql {
				t.Fatalf("Postgres SQL changed\nwant: %s\ngot:  %s", s.sql, got)
			}
		})
	}
}
