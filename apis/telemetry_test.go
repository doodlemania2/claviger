package apis_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/telemetry"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const testSuperuserToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"

// These tests install a process-wide telemetry instance, so they must not run
// in parallel with each other (they are deliberately not t.Parallel()).

func installTelemetry(t *testing.T, opts telemetry.Options) {
	t.Helper()
	tel, err := telemetry.Setup(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	telemetry.SetActive(tel)
	t.Cleanup(func() {
		telemetry.SetActive(nil)
		_ = tel.Shutdown(context.Background())
	})
}

func TestTelemetryServerAndDBSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	installTelemetry(t, telemetry.Options{SpanExporter: exp})

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const secretPassword = "hunter2-very-secret-pw"
	const queryToken = "tok_query_secret_9876"

	scenarios := []tests.ApiScenario{
		{
			Name:   "records list produces a server span with a DB child",
			Method: http.MethodGet,
			URL:    "/api/collections/demo1/records?token=" + queryToken + "&filter=" + "text%3D%27" + queryToken + "%27",
			Headers: map[string]string{
				"Authorization": testSuperuserToken,
				"traceparent":   "00-" + traceID + "-00f067aa0ba902b7-01",
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"items"`,
			},
			ExpectedEvents: map[string]int{"OnRecordsListRequest": 1},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				spans := exp.GetSpans()

				var serverID string
				for _, s := range spans {
					if s.Name == "GET /api/collections/{collection}/records" {
						serverID = s.SpanContext.SpanID().String()
						if got := s.SpanContext.TraceID().String(); got != traceID {
							t.Fatalf("traceparent not continued: trace id %s", got)
						}
						if got := s.Parent.SpanID().String(); got != "00f067aa0ba902b7" {
							t.Fatalf("remote parent not set: %s", got)
						}
						if s.SpanKind.String() != "server" {
							t.Fatalf("unexpected kind %v", s.SpanKind)
						}
					}
				}
				if serverID == "" {
					t.Fatalf("no server span named by route template; got %v", spanNames(exp))
				}

				dbChildren := 0
				for _, s := range spans {
					if s.Parent.SpanID().String() == serverID && s.SpanKind.String() == "client" {
						dbChildren++
					}
				}
				if dbChildren == 0 {
					t.Fatalf("no DB child span under the server span; got %v", spanNames(exp))
				}

				// nothing sensitive in any attribute, name or event
				var dump strings.Builder
				for _, s := range spans {
					dump.WriteString(s.Name)
					for _, a := range s.Attributes {
						fmt.Fprintf(&dump, " %s=%s", a.Key, a.Value.Emit())
					}
					for _, ev := range s.Events {
						dump.WriteString(ev.Name)
					}
				}
				for _, secret := range []string{queryToken, testSuperuserToken, "eyJhbGci"} {
					if strings.Contains(dump.String(), secret) {
						t.Fatalf("span data leaks %q:\n%s", secret, dump.String())
					}
				}
				if !strings.Contains(dump.String(), "http.response.status_code=200") {
					t.Fatalf("status code missing:\n%s", dump.String())
				}
			},
		},
		{
			Name:   "failed password auth leaks neither identity nor password",
			Method: http.MethodPost,
			URL:    "/api/collections/users/auth-with-password",
			Body:   strings.NewReader(`{"identity":"test@example.com","password":"` + secretPassword + `"}`),
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"data":{}`,
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				for _, s := range exp.GetSpans() {
					all := s.Name
					for _, a := range s.Attributes {
						all += " " + string(a.Key) + "=" + a.Value.Emit()
					}
					if strings.Contains(all, secretPassword) || strings.Contains(all, "test@example.com") {
						t.Fatalf("span leaks credentials: %s", all)
					}
				}
			},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}

func spanNames(exp *tracetest.InMemoryExporter) []string {
	var names []string
	for _, s := range exp.GetSpans() {
		names = append(names, s.Name+"("+s.SpanKind.String()+")")
	}
	return names
}

func TestHealthIsNotTraced(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	installTelemetry(t, telemetry.Options{SpanExporter: exp})

	(&tests.ApiScenario{
		Name: "health", Method: http.MethodGet, URL: "/api/health", ExpectedStatus: 200,
		ExpectedContent: []string{`"code":200`},
		ExpectedEvents:  map[string]int{"*": 0},
	}).Test(t)

	for _, s := range exp.GetSpans() {
		if strings.Contains(s.Name, "/api/health") {
			t.Fatalf("health probe must not be traced: %s", s.Name)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	const token = "s3cret-scrape-token"

	t.Run("404 when CLAVIGER_METRICS_TOKEN is unset", func(t *testing.T) {
		t.Setenv(telemetry.MetricsTokenEnv, "")
		for _, h := range []map[string]string{nil, {"Authorization": "Bearer " + token}} {
			(&tests.ApiScenario{
				Name: "unset", Method: http.MethodGet, URL: "/api/metrics", Headers: h,
				ExpectedStatus: 404, ExpectedContent: []string{`"data":{}`},
				ExpectedEvents: map[string]int{"*": 0},
			}).Test(t)
		}
	})

	t.Run("token set", func(t *testing.T) {
		t.Setenv(telemetry.MetricsTokenEnv, token)
		installTelemetry(t, telemetry.Options{})

		for name, h := range map[string]map[string]string{
			"missing":     nil,
			"wrong":       {"Authorization": "Bearer nope"},
			"no scheme":   {"Authorization": token},
			"prefix only": {"Authorization": "Bearer " + token[:5]},
			"superuser":   {"Authorization": testSuperuserToken},
		} {
			(&tests.ApiScenario{
				Name: "401 " + name, Method: http.MethodGet, URL: "/api/metrics", Headers: h,
				ExpectedStatus: 401, ExpectedContent: []string{`"data":{}`},
				NotExpectedContent: []string{"go_goroutines"},
				ExpectedEvents:     map[string]int{"*": 0},
			}).Test(t)
		}

		// generate traffic: a request, a DB read and a failed auth
		(&tests.ApiScenario{
			Name: "traffic", Method: http.MethodPost, URL: "/api/collections/users/auth-with-password",
			Body:           strings.NewReader(`{"identity":"test@example.com","password":"wrong-password"}`),
			Headers:        map[string]string{"Content-Type": "application/json"},
			ExpectedStatus: 400, ExpectedContent: []string{`"data":{}`},
		}).Test(t)

		(&tests.ApiScenario{
			Name: "200 with the right token", Method: http.MethodGet, URL: "/api/metrics",
			Headers:        map[string]string{"Authorization": "Bearer " + token},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				"# TYPE claviger_http_server_requests_total counter",
				`http_route="/api/collections/{collection}/auth-with-password"`,
				`http_response_status_class="4xx"`,
				"http_server_request_duration_seconds_bucket",
				"http_server_active_requests",
				"claviger_db_query_duration_seconds_bucket",
				"claviger_realtime_subscribers",
				`claviger_auth_attempts_total{`,
				`auth_method="password"`,
				`auth_outcome="failure"`,
				"go_goroutines",
				"go_memory_heap_alloc_bytes",
				"go_gc_cycles_total",
				"process_uptime_seconds",
			},
			NotExpectedContent: []string{token, "wrong-password", "test@example.com"},
			ExpectedEvents:     map[string]int{"*": 0},
		}).Test(t)
	})
}

// TestTelemetryDisabledByDefault pins the opt-out contract: with nothing
// configured there is no instance, the DB driver is not wrapped and the
// metrics route does not exist.
func TestTelemetryDisabledByDefault(t *testing.T) {
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", telemetry.MetricsTokenEnv,
	} {
		t.Setenv(k, "")
	}

	if telemetry.Configured() {
		t.Fatal("Configured() must be false with no env set")
	}

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	if telemetry.Active() != nil {
		t.Fatal("telemetry must not be active by default")
	}
	if http.DefaultClient.Transport != nil {
		t.Fatal("http.DefaultClient must be left alone")
	}
	for name, db := range map[string]dbx.Builder{"data": app.ConcurrentDB(), "aux": app.AuxConcurrentDB()} {
		if got := fmt.Sprintf("%T", db.(*dbx.DB).DB().Driver()); strings.Contains(got, "telemetry") {
			t.Fatalf("%s DB driver is wrapped: %s", name, got)
		}
	}

	(&tests.ApiScenario{
		Name: "no metrics route", Method: http.MethodGet, URL: "/api/metrics",
		Headers:        map[string]string{"Authorization": "Bearer anything"},
		ExpectedStatus: 404, ExpectedContent: []string{`"data":{}`},
		ExpectedEvents: map[string]int{"*": 0},
	}).Test(t)

}
