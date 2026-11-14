package core_test

import (
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #86): the state of a grant.

func TestBreakGlassState(t *testing.T) {
	app := newOrgFixture(t).app

	col, _ := app.FindCollectionByNameOrId(core.CollectionNameBreakGlass)
	now := time.Now().UTC()

	mk := func(mod func(*core.Record)) string {
		g := core.NewRecord(col)
		g.Set("requestedAt", now.Add(-time.Hour))
		mod(g)

		return core.BreakGlassState(g, now)
	}

	if s := mk(func(*core.Record) {}); s != core.BreakGlassPending {
		t.Fatal(s)
	}

	if s := mk(func(g *core.Record) { g.Set("requestedAt", now.Add(-25*time.Hour)) }); s != core.BreakGlassLapsed {
		t.Fatal(s)
	}

	if s := mk(func(g *core.Record) { g.Set("revoked", true) }); s != core.BreakGlassRevoked {
		t.Fatal(s)
	}

	if s := mk(func(g *core.Record) {
		g.Set("activeFrom", now.Add(-time.Minute))
		g.Set("expires", now.Add(time.Hour))
	}); s != core.BreakGlassActive {
		t.Fatal(s)
	}

	if s := mk(func(g *core.Record) {
		g.Set("activeFrom", now.Add(-2*time.Hour))
		g.Set("expires", now.Add(-time.Hour))
	}); s != core.BreakGlassExpired {
		t.Fatal(s)
	}
}
