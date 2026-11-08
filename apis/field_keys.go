package apis

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/routine"
)

// Fork-local (Claviger #75, M3): field key maintenance, run inside the
// serving process.
//
//	GET  /api/field-keys           the keys and the pending work of every encrypted field, plus the current/last job
//	GET  /api/field-keys/job       the current/last job only (cheap, for polling)
//	POST /api/field-keys/rotate    {collection?, field?, batchSize?}  new data key + re-encrypt (background job)
//	POST /api/field-keys/rewrap    {}                                  re-wrap the data keys under the primary KEK (background job)
//	POST /api/field-keys/convert   {collection, field?, batchSize?}   encrypt legacy plaintext values (background job)
//	POST /api/field-keys/prune     {collection?, field?}               delete unused retired keys (background job)
//	POST /api/field-keys/strict    {collection, field, strict}         strict mode on/off (synchronous)
//
// All of them are superuser-only and never reachable with an API key. The
// background jobs run one at a time, write one audit entry each and stop
// between two batches when the server shuts down; running the same operation
// again continues where it stopped.

const fieldKeyJobStoreKey = "__clavigerFieldKeyJob__"

func bindFieldKeysApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	g := rg.Group("/field-keys")
	g.GET("", fieldKeysStatus).Bind(RequireSuperuserAuth())
	g.GET("/job", fieldKeysJob).Bind(RequireSuperuserAuth())
	g.POST("/rotate", fieldKeysStart("rotate")).Bind(RequireSuperuserAuth())
	g.POST("/rewrap", fieldKeysStart("rewrap")).Bind(RequireSuperuserAuth())
	g.POST("/convert", fieldKeysStart("convert")).Bind(RequireSuperuserAuth())
	g.POST("/prune", fieldKeysStart("prune")).Bind(RequireSuperuserAuth())
	g.POST("/strict", fieldKeysStrict).Bind(RequireSuperuserAuth())

	// stop a running job between two batches when the server goes down
	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		if job := currentFieldKeyJob(e.App); job != nil {
			job.cancel()
		}
		return e.Next()
	})
}

type fieldKeyJobBody struct {
	Collection string `json:"collection" form:"collection"`
	Field      string `json:"field" form:"field"`
	BatchSize  int    `json:"batchSize" form:"batchSize"`
	Strict     bool   `json:"strict" form:"strict"`
}

type fieldKeyJob struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	id       string
	op       string
	state    string // running | done | failed
	started  time.Time
	finished time.Time
	progress core.FieldKeyProgress
	result   any
	err      string
}

func (j *fieldKeyJob) view() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()

	m := map[string]any{
		"id":       j.id,
		"op":       j.op,
		"state":    j.state,
		"started":  j.started.UTC().Format(time.RFC3339),
		"progress": j.progress,
		"result":   j.result,
		"error":    j.err,
	}
	if !j.finished.IsZero() {
		m["finished"] = j.finished.UTC().Format(time.RFC3339)
	}
	return m
}

func (j *fieldKeyJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == "running"
}

func currentFieldKeyJob(app core.App) *fieldKeyJob {
	j, _ := app.Store().Get(fieldKeyJobStoreKey).(*fieldKeyJob)
	return j
}

func fieldKeysJobView(app core.App) any {
	if j := currentFieldKeyJob(app); j != nil {
		return j.view()
	}
	return nil
}

func fieldKeysPrecheck(e *core.RequestEvent) error {
	if APIKeyFromRequest(e) != nil {
		return apiKeyForbidden(e)
	}
	return nil
}

func fieldKeysJob(e *core.RequestEvent) error {
	if err := fieldKeysPrecheck(e); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"job": fieldKeysJobView(e.App)})
}

func fieldKeysStatus(e *core.RequestEvent) error {
	if err := fieldKeysPrecheck(e); err != nil {
		return err
	}

	report, err := core.FieldKeysStatus(e.Request.Context(), e.App)
	if err != nil {
		return e.InternalServerError("Failed to load the field key status.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"status": report, "job": fieldKeysJobView(e.App)})
}

func fieldKeysStart(op string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if err := fieldKeysPrecheck(e); err != nil {
			return err
		}

		body := fieldKeyJobBody{}
		if e.Request.ContentLength != 0 {
			if err := e.BindBody(&body); err != nil {
				return e.BadRequestError("An error occurred while loading the submitted data.", err)
			}
		}

		if op == "convert" && body.Collection == "" {
			return e.BadRequestError("A collection is required.", nil)
		}
		if body.Field != "" && body.Collection == "" {
			return e.BadRequestError("A field needs a collection.", nil)
		}

		if prev := currentFieldKeyJob(e.App); prev != nil && prev.isRunning() {
			return e.JSON(http.StatusConflict, map[string]any{
				"status":  http.StatusConflict,
				"message": "Another field key operation is already running.",
				"data":    map[string]any{},
				"job":     prev.view(),
			})
		}

		ctx, cancel := context.WithCancel(auditContext(e))
		job := &fieldKeyJob{
			cancel:  cancel,
			id:      core.GenerateDefaultRandomId(),
			op:      op,
			state:   "running",
			started: time.Now(),
		}
		e.App.Store().Set(fieldKeyJobStoreKey, job)

		opts := core.FieldKeyOptions{
			Collection: body.Collection,
			Field:      body.Field,
			BatchSize:  body.BatchSize,
			Progress: func(p core.FieldKeyProgress) {
				job.mu.Lock()
				job.progress = p
				job.mu.Unlock()
			},
		}

		app := e.App
		routine.FireAndForget(func() {
			defer cancel()

			var result any
			var err error

			switch op {
			case "rotate":
				result, err = core.RotateFieldKeys(ctx, app, opts)
			case "rewrap":
				result, err = core.RewrapFieldKeys(ctx, app)
			case "convert":
				result, err = core.ConvertFieldValues(ctx, app, opts)
			case "prune":
				result, err = core.PruneFieldKeys(ctx, app, opts)
			}

			job.mu.Lock()
			defer job.mu.Unlock()
			job.finished = time.Now()
			job.result = result
			if err != nil {
				job.state = "failed"
				job.err = err.Error()
				if errors.Is(err, context.Canceled) {
					job.err = "stopped (the server is shutting down); run it again to continue"
				}
				return
			}
			job.state = "done"
		})

		return e.JSON(http.StatusAccepted, map[string]any{"job": job.view()})
	}
}

func fieldKeysStrict(e *core.RequestEvent) error {
	if err := fieldKeysPrecheck(e); err != nil {
		return err
	}

	body := fieldKeyJobBody{}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}

	if body.Collection == "" || body.Field == "" {
		return e.BadRequestError("A collection and a field are required.", nil)
	}

	if prev := currentFieldKeyJob(e.App); prev != nil && prev.isRunning() {
		return e.BadRequestError("Another field key operation is running; wait for it to finish.", nil)
	}

	if err := core.SetFieldKeysStrict(auditContext(e), e.App, body.Collection, body.Field, body.Strict); err != nil {
		if errors.Is(err, core.ErrFieldKeyOpRunning) {
			return e.BadRequestError(err.Error(), nil)
		}
		return e.BadRequestError("Failed to change the strict mode: "+err.Error(), nil)
	}

	return e.JSON(http.StatusOK, map[string]any{"collection": body.Collection, "field": body.Field, "strict": body.Strict})
}
