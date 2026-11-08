//go:build postgres

package pgtests

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// Every supported strftime() format and modifier gives the same text as
// SQLite, for dates around the edges (year end, ISO week 53, leap day).
func TestStrftimeMatchesSQLite(t *testing.T) {
	sqliteApp := newSQLiteApp(t)
	pgApp := newApp(t)

	fb := dialect.Postgres.(dialect.FunctionBuilder)

	dates := []string{
		"2024-02-05 03:04:05.678Z",
		"2023-12-31 23:59:59.999Z",
		"2024-01-01 00:00:00.000Z",
		"2021-01-03 12:00:00.000Z", // Sunday, ISO week 53 of 2020
		"2024-02-29 13:14:15.000Z",
		"2024-12-30 06:07:08.009Z", // ISO week 1 of 2025
		"2024-07-04",
		"2024-07-04 09:05",
	}

	formats := []string{
		"%Y", "%m", "%d", "%e", "%H", "%I", "%k", "%l", "%M", "%S", "%f", "%j", "%p", "%P",
		"%w", "%u", "%s", "%U", "%W", "%V", "%G", "%g", "%F", "%T", "%R", "%%",
		"%Y-%m-%dT%H:%M:%S", "Q%m/%Y week %W day %w", `100%% "quoted" it's`,
	}

	for _, format := range formats {
		for _, date := range dates {
			check(t, sqliteApp, pgApp, fb, format, date, nil)
		}
	}

	modifiers := [][]string{
		{"start of day"}, {"start of month"}, {"start of year"},
		{"+1 day"}, {"-1 day"}, {"+36 hours"}, {"-90 minutes"}, {"+45 seconds"}, {"+1 year"}, {"-1 year"},
		{"+1 month"}, {"-1 month"}, {"+2.5 days"},
		{"start of month", "+1 month", "-1 day"}, {"start of year", "+6 months"},
		{"weekday 0"}, {"weekday 3"}, {"weekday 6"}, {"+3 days", "weekday 1"},
		{"utc"}, {"subsec"},
	}

	for _, mods := range modifiers {
		for _, date := range dates[:6] {
			// 29 Feb + 1 year and 31st + month overflow differ by design (docs/datastores.md)
			if (strings.Contains(strings.Join(mods, ","), "1 year") && strings.Contains(date, "-02-29")) ||
				(strings.Contains(strings.Join(mods, ","), "month") && (strings.Contains(date, "-31 ") || strings.Contains(date, "-30 ") || strings.Contains(date, "-29 "))) {
				continue
			}
			check(t, sqliteApp, pgApp, fb, "%Y-%m-%d %H:%M:%f", date, mods)
		}
	}

	// numbers: julian day and unix time
	for _, tc := range []struct {
		value string
		mods  []string
	}{
		{"2460324.5", nil},
		{"2460324.75", nil},
		{"1705312800", []string{"unixepoch"}},
		{"1705312800.5", []string{"unixepoch"}},
		{"0", []string{"unixepoch"}},
	} {
		sq := "SELECT strftime('%Y-%m-%d %H:%M:%f', " + tc.value
		for _, m := range tc.mods {
			sq += ", '" + m + "'"
		}
		sq += ")"
		var want string
		if err := sqliteApp.DB().NewQuery(sq).Row(&want); err != nil {
			t.Fatal(err)
		}

		expr, err := fb.StrFTime("%Y-%m-%d %H:%M:%f", tc.value, true, tc.mods)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if err := pgApp.DB().NewQuery("SELECT " + expr).Row(&got); err != nil {
			t.Fatalf("%s: %v", tc.value, err)
		}
		if got != want {
			t.Errorf("number %s %v: sqlite %q, postgres %q", tc.value, tc.mods, want, got)
		}
	}

	// empty and invalid dates are NULL
	for _, bad := range []string{"", "not a date", "2024-13-45"} {
		expr, err := fb.StrFTime("%Y", "'"+bad+"'", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got *string
		if err := pgApp.DB().NewQuery("SELECT " + expr).Row(&got); err != nil {
			t.Fatalf("%q: %v", bad, err)
		}
		if got != nil {
			t.Errorf("strftime of %q = %q, want NULL", bad, *got)
		}
	}

	// the session time zone does not matter
	for _, zone := range []string{"UTC", "America/Los_Angeles", "Asia/Kolkata"} {
		if _, err := pgApp.DB().NewQuery("SET TIME ZONE '" + zone + "'").Execute(); err != nil {
			t.Fatal(err)
		}
		expr, _ := fb.StrFTime("%Y-%m-%d %H", "'2024-03-10 07:00:00.000Z'", false, nil)
		var got string
		if err := pgApp.DB().NewQuery("SELECT " + expr).Row(&got); err != nil {
			t.Fatal(err)
		}
		if got != "2024-03-10 07" {
			t.Errorf("zone %s: %q", zone, got)
		}
	}
	_ = core.DialectOf
}

func check(t *testing.T, sqliteApp, pgApp *core.BaseApp, fb dialect.FunctionBuilder, format, date string, mods []string) {
	t.Helper()

	sq := "SELECT strftime({:f}, {:d}"
	for i := range mods {
		sq += ", {:m" + string(rune('a'+i)) + "}"
	}
	sq += ")"
	params := map[string]any{"f": format, "d": date}
	for i, m := range mods {
		params["m"+string(rune('a'+i))] = m
	}

	var want *string
	if err := sqliteApp.DB().NewQuery(sq).Bind(params).Row(&want); err != nil {
		t.Fatal(err)
	}

	expr, err := fb.StrFTime(format, "{:d}", false, mods)
	if err != nil {
		t.Errorf("%q %v: %v", format, mods, err)
		return
	}
	var got *string
	if err := pgApp.DB().NewQuery("SELECT " + expr).Bind(map[string]any{"d": date}).Row(&got); err != nil {
		t.Errorf("%q %q %v: %v", format, date, mods, err)
		return
	}

	switch {
	case want == nil && got == nil:
	case want == nil || got == nil:
		t.Errorf("strftime(%q, %q, %v): sqlite %v, postgres %v", format, date, mods, want, got)
	case *want != *got:
		t.Errorf("strftime(%q, %q, %v): sqlite %q, postgres %q", format, date, mods, *want, *got)
	}
}
