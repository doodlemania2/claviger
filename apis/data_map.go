package apis

import (
	"net/http"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #88, 88-0): data map administration.
//
//	GET /api/data-map/unmapped   the collections without a _dataMap entry
//
// Superuser only. A route rather than a CLI subcommand because it is
// read-only, needs no new command wiring, and the admin UI (88-5) will call
// the same thing. The entries themselves are ordinary records of the _dataMap
// collection (records API, superusers).
func bindDataMapApi(_ core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	g := rg.Group("/data-map")
	g.GET("/unmapped", dataMapUnmapped).Bind(RequireSuperuserAuth())
}

func dataMapUnmapped(e *core.RequestEvent) error {
	items, err := core.UnmappedCollections(e.App)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"totalItems": len(items),
		"items":      items,
	})
}
