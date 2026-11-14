package apis

import (
	"net/http"
	"strconv"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #87, 87-5): read routes for the register screens.
//
//	GET /api/registers?org={id}            the registers, with what the caller may do in that org (a superuser may leave org out)
//	GET /api/registers/{collection}?org=&state=&page=&perPage=   the entries of a register, newest first
//	GET /api/registers/_verify             superuser: recompute every hash chain and report the breaks
//
// They only read. Finalizing, withdrawing and annotating stay on their own routes.

func registerSummaryJSON(app core.App, cfg *core.PermanentConfig) map[string]any {
	return map[string]any{
		"collection":           cfg.Collection,
		"annotationCollection": cfg.AnnotationCollection,
		"signers":              cfg.Signers,
		"chain":                cfg.Chain,
		"finalizeField":        cfg.FinalizeField,
		"fields":               core.RegisterDisplayFields(app, cfg),
	}
}

// registerAccess resolves the org of a request: an org id is required, except
// for a superuser, who may read across orgs.
func registerAccess(e *core.RequestEvent) (*core.RegisterAccess, string, error) {
	orgId := e.Request.URL.Query().Get("org")

	if orgId == "" {
		if !e.HasSuperuserAuth() {
			return nil, "", e.BadRequestError("Say which organization with ?org=.", nil)
		}

		return &core.RegisterAccess{Staff: true, Superuser: true}, "", nil
	}

	acc, err := core.RegisterAccessFor(e.App, orgId, e.Auth)
	if err != nil {
		return nil, "", orgHTTP(e, err)
	}

	return acc, orgId, nil
}

func registerList(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	acc, _, err := registerAccess(e)
	if err != nil {
		return err
	}

	items := []map[string]any{}
	for _, cfg := range core.PermanentConfigs(e.App) {
		items = append(items, registerSummaryJSON(e.App, cfg))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items, "registrar": acc.Registrar, "manager": acc.Manager, "superuser": acc.Superuser})
}

func registerEntries(e *core.RequestEvent) error {
	if err := registerGate(e); err != nil {
		return err
	}

	cfg := core.PermanentConfigFor(e.App, e.Request.PathValue("collection"))
	if cfg == nil {
		return e.NotFoundError("Register not found.", nil)
	}

	_, orgId, err := registerAccess(e)
	if err != nil {
		return err
	}

	q := e.Request.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	perPage, _ := strconv.Atoi(q.Get("perPage"))

	list, err := core.ListRegisterEntries(e.App, cfg, core.RegisterListFilter{Org: orgId, State: q.Get("state"), Page: page, PerPage: perPage})
	if err != nil {
		return orgHTTP(e, err)
	}

	items := make([]map[string]any, 0, len(list.Items))
	for _, entry := range list.Items {
		items = append(items, registerEntryJSON(e.App, cfg, entry))
	}

	return e.JSON(http.StatusOK, map[string]any{
		"page": list.Page, "perPage": list.PerPage, "totalItems": list.TotalItems, "items": items,
	})
}

func registerVerify(e *core.RequestEvent) error {
	res, err := core.VerifyRegisters(e.App)
	if err != nil {
		return e.InternalServerError("Could not verify a register.", err)
	}

	ok := true
	for _, v := range res {
		if !v.OK() {
			ok = false
		}
	}

	if res == nil {
		res = []*core.RegisterVerification{}
	}

	return e.JSON(http.StatusOK, map[string]any{"ok": ok, "registers": res})
}
