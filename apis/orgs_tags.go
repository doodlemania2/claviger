package apis

import (
	"net/http"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #79b): the tags of an org.
//
//	GET    /api/orgs/{id}/tags            members
//	POST   /api/orgs/{id}/tags            {name, description?} (owner/admin)
//	DELETE /api/orgs/{id}/tags/{tag}      (owner/admin); the tag leaves every member
func bindOrgTagsApi(orgs *router.RouterGroup[*core.RequestEvent]) {
	orgs.GET("/{id}/tags", orgTagsList)
	orgs.POST("/{id}/tags", orgTagsCreate)
	orgs.DELETE("/{id}/tags/{tag}", orgTagsDelete)
}

func orgTagJSON(tag *core.Record) map[string]any {
	return map[string]any{
		"id":          tag.Id,
		"name":        tag.GetString("name"),
		"description": tag.GetString("description"),
	}
}

func orgTagsList(e *core.RequestEvent) error {
	org, _, err := orgSession(e)
	if err != nil {
		return err
	}

	if org.GetBool("archived") && !e.Auth.IsSuperuser() {
		return e.NotFoundError("Organization not found.", nil)
	}

	tags, err := e.App.FindRecordsByFilter(core.CollectionNameOrgRoles, "org={:org}", "name", 0, 0, dbx.Params{"org": org.Id})
	if err != nil {
		return e.InternalServerError("", err)
	}

	items := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		items = append(items, orgTagJSON(t))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func orgTagsCreate(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	var body struct {
		Name        string `json:"name" form:"name"`
		Description string `json:"description" form:"description"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	tag, err := core.CreateOrgTag(e.App, auditContext(e), actor, org, body.Name, body.Description)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, orgTagJSON(tag))
}

func orgTagsDelete(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	tag, err := e.App.FindRecordById(core.CollectionNameOrgRoles, e.Request.PathValue("tag"))
	if err != nil || tag.GetString("org") != org.Id {
		return e.NotFoundError("Tag not found.", nil)
	}

	if err := core.DeleteOrgTag(e.App, auditContext(e), actor, org, tag); err != nil {
		return orgHTTP(e, err)
	}

	return e.NoContent(http.StatusNoContent)
}
