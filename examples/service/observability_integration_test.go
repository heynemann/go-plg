//go:build integration

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/internal/authorrepo"
)

// couponToken is the coupon API's bearer token: the seasonal rule's secret coupon_api_token.
const couponToken = "cpn-7Hq2xV9bLw4R" //nolint:gosec // G101: the test coupon API's made-up token

// couponRequest is what the coupon API received of a request.
type couponRequest struct {
	authorization string
	traceparent   string
	path          string
}

// couponAPI is a coupon API for the seasonal rule: GET /coupons/{code} with the bearer token couponToken answers
// AUTUMN with 5% off, refuses REVOKED with a 403 echoing the token it got (as careless APIs do) and does not know
// any other code (404); without the token it answers 401.
type couponAPI struct {
	*httptest.Server

	// requests receives every request the API got; the tests read each one.
	requests chan couponRequest
	// host is the API's host:port, for -coupon-api.
	host string
}

// newCouponAPI starts a coupon API, closed when the test ends.
func newCouponAPI(t *testing.T) *couponAPI {
	t.Helper()

	requests := make(chan couponRequest, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /coupons/{code}", func(writer http.ResponseWriter, req *http.Request) {
		authorization := req.Header.Get("Authorization")
		requests <- couponRequest{
			authorization: authorization, traceparent: req.Header.Get("Traceparent"), path: req.URL.Path,
		}

		token, _ := strings.CutPrefix(authorization, "Bearer ")

		switch code := req.PathValue("code"); {
		case token != couponToken:
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
		case code == "AUTUMN":
			writer.Header().Set("Content-Type", "application/json")

			_, err := writer.Write([]byte(`{"percent":5}`))
			assert.NoError(t, err)
		case code == "REVOKED":
			http.Error(writer, "token "+token+" may not read REVOKED", http.StatusForbidden)
		default:
			http.NotFound(writer, req)
		}
	})

	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)

	parsed, err := url.Parse(api.URL)
	require.NoError(t, err)

	return &couponAPI{Server: api, requests: requests, host: parsed.Host}
}

// environ returns the environment variables of the seasonal rule's secrets: the API's URL and token.
func (api *couponAPI) environ() []string {
	return []string{
		"PRICING_SECRET_SEASONAL_COUPON_API_URL=" + api.URL,
		"PRICING_SECRET_SEASONAL_COUPON_API_TOKEN=" + couponToken,
	}
}

// received requires the API to have received a request since the last one read, and returns it. The API records a
// request before answering it, so once the quote that made it is answered, it is there.
func (api *couponAPI) received(t *testing.T) couponRequest {
	t.Helper()

	select {
	case req := <-api.requests:
		return req
	default:
		require.FailNow(t, "the coupon API received no request")

		return couponRequest{}
	}
}

// addSeasonal copies the seasonal rule of the example author repository into the plugin directory dir.
func addSeasonal(t *testing.T, dir string) {
	t.Helper()

	seasonal := filepath.Join(authorrepo.Examples(t), "rules", "seasonal")
	require.NoError(t, os.CopyFS(filepath.Join(dir, "seasonal"), os.DirFS(seasonal)))
}

// A quote request carrying a traceparent is one trace: the service's server span continues it, the seasonal rule's
// call is a child of that span and its request to the coupon API a child of the call, and the API receives that
// request's span as its traceparent, with the bearer token the rule read from its secret. The token (and the API's
// URL, the rule's other secret) appear nowhere in what the service reports: its log, the events and the admin
// JSON, and the exported spans, where plg writes [secret <name>] instead, as it does in the error of a rule whose
// message would carry the token.
func TestServiceQuoteIsOneTraceKeepingItsSecrets(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.WithoutCancel(t.Context()))) })

	api := newCouponAPI(t)
	rules := t.TempDir()
	addSeasonal(t, rules)

	vars := map[string]string{"PRICING_ADMIN_TOKEN": adminToken}

	for _, kv := range api.environ() {
		name, value, _ := strings.Cut(kv, "=")
		vars[name] = value
	}

	var logs bytes.Buffer // written under the service's log handler, read once the service exited

	svc := startIn(t, process{stderr: &logs, tracerProvider: provider, lookupEnv: environment(vars)},
		"-plugins", rules, "-coupon-api", api.host, "-coupon-api-private")

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)

	clientSpan, err := trace.SpanIDFromHex("00f067aa0ba902b7") // the caller's span, which the traceparent carries
	require.NoError(t, err)

	traceparent := "00-" + traceID.String() + "-" + clientSpan.String() + "-01"

	got := decodeQuote(t, post(t, svc.url,
		`{"customer":"bob","coupon":"AUTUMN","items":[{"sku":"tea","quantity":10,"unitCents":450}]}`,
		http.Header{"Traceparent": {traceparent}}))
	assert.Equal(t, quote{
		Applied: "pricing/seasonal", SubtotalCents: 4500, TotalCents: 4275,
		Discounts: []ruleResult{{Rule: "pricing/seasonal", Cents: 225, Reason: "coupon AUTUMN: 5% off"}},
	}, got, "the coupon API's 5%")

	asked := api.received(t)
	assert.Equal(t, "Bearer "+couponToken, asked.authorization, "the rule's secret")
	assert.Equal(t, "/coupons/AUTUMN", asked.path)

	got = quoteFor(t, svc.url,
		`{"customer":"bob","coupon":"REVOKED","items":[{"sku":"tea","quantity":10,"unitCents":450}]}`)
	assert.Equal(t, []ruleResult{{
		Rule:  "pricing/seasonal",
		Error: "the coupon API answered 403 for REVOKED: token [secret coupon_api_token] may not read REVOKED",
	}}, got.Discounts, "the API echoed the token into the rule's error")
	assert.Equal(t, "Bearer "+couponToken, api.received(t).authorization)

	secrets := []string{couponToken, api.URL}

	for _, path := range []string{
		"/admin/plg/plugins", "/admin/plg/plugins/pricing/seasonal", "/admin/plg/plugins/pricing/seasonal/events",
	} {
		admin := adminDo(t, http.MethodGet, svc.url+path, adminToken, http.NoBody)
		require.Equal(t, http.StatusOK, admin.status, "%s: %s", path, admin.body)

		for _, secret := range secrets {
			assert.NotContains(t, string(admin.body), secret, path)
		}
	}

	svc.stop() // every span ended (the server's once its handler returned), every log record and event written

	for _, secret := range secrets {
		assert.NotContains(t, logs.String(), secret, "the service's log")
	}

	assert.Contains(t, logs.String(), `url="[secret coupon_api_url]/coupons/AUTUMN"`, "plg logged the request")

	spans := exporter.GetSpans()
	for i := range spans {
		for _, secret := range secrets {
			assert.NotContains(t, spanText(&spans[i]), secret, spans[i].Name)
		}
	}

	server := spanNamed(t, spans, traceID, "POST /quote")
	assert.Equal(t, trace.SpanKindServer, server.SpanKind)
	assert.Equal(t, clientSpan, server.Parent.SpanID(), "the server span continues the client's")
	assert.True(t, server.Parent.IsRemote())

	call := spanNamed(t, spans, traceID, "plg.call pricing/seasonal.Discount")
	assert.Equal(t, server.SpanContext.SpanID(), call.Parent.SpanID(), "the rule's call is the request's child")

	request := spanNamed(t, spans, traceID, "plg.http GET")
	assert.Equal(t, call.SpanContext.SpanID(), request.Parent.SpanID(), "the API request is the call's child")
	assert.Equal(t, "[secret coupon_api_url]/coupons/AUTUMN", attrs(&request)["url.full"])
	assert.Equal(t, "00-"+traceID.String()+"-"+request.SpanContext.SpanID().String()+"-01",
		asked.traceparent, "the API received the request's span, in the client's trace")

	revoked := spansNamed(spans, "plg.call pricing/seasonal.Discount")
	require.Len(t, revoked, 2)
	assert.Equal(t, codes.Error, revoked[1].Status.Code)
	assert.Contains(t, revoked[1].Status.Description, "token [secret coupon_api_token] may not read REVOKED",
		"the failed call's span carries the scrubbed error")
}

// spansNamed returns the spans named name, in the order they ended.
func spansNamed(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var out tracetest.SpanStubs

	for i := range spans {
		if spans[i].Name == name {
			out = append(out, spans[i])
		}
	}

	return out
}

// spanNamed requires the trace id to hold one span named name, and returns it.
func spanNamed(t *testing.T, spans tracetest.SpanStubs, id trace.TraceID, name string) tracetest.SpanStub {
	t.Helper()

	var found tracetest.SpanStubs

	named := spansNamed(spans, name)
	for i := range named {
		if named[i].SpanContext.TraceID() == id {
			found = append(found, named[i])
		}
	}

	require.Len(t, found, 1, "%s in trace %s", name, id)

	return found[0]
}

// attrs returns a span's attributes as strings.
func attrs(span *tracetest.SpanStub) map[string]string {
	out := map[string]string{}

	for _, kv := range span.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}

	return out
}

// spanText returns everything a span says as text: its name, attributes, status and events.
func spanText(span *tracetest.SpanStub) string {
	var text strings.Builder

	text.WriteString(span.Name + "\n" + span.Status.Description + "\n")

	for _, kv := range span.Attributes {
		text.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
	}

	for _, event := range span.Events {
		text.WriteString(event.Name + "\n")

		for _, kv := range event.Attributes {
			text.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
		}
	}

	return text.String()
}

// The rules' secrets come from the environment, each rule's from its own variables.
func TestEnvSecrets(t *testing.T) {
	t.Parallel()

	resolve := envSecrets(environment(map[string]string{
		"PRICING_SECRET_SEASONAL_COUPON_API_TOKEN": "s3asonal",
		"PRICING_SECRET_MY_RULE_API_KEY_2":         "k3y",
		"PRICING_SECRET_VIP_EMPTY":                 "",
	}))

	tests := map[string]struct {
		key, name string
		want      string
		missing   string // the variable the error names, when the read fails
	}{
		"a rule's secret":   {key: "pricing/seasonal", name: "coupon_api_token", want: "s3asonal"},
		"names upper-cased": {key: "pricing/my-rule", name: "api.key-2", want: "k3y"},
		"unset": {
			key:     "pricing/seasonal",
			name:    "coupon_api_url",
			missing: "PRICING_SECRET_SEASONAL_COUPON_API_URL",
		},
		"empty": {key: "pricing/vip", name: "empty", missing: "PRICING_SECRET_VIP_EMPTY"},
		"another rule's one": {
			key:     "pricing/vip",
			name:    "coupon_api_token",
			missing: "PRICING_SECRET_VIP_COUPON_API_TOKEN",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := resolve(t.Context(), plg.PluginInfo{Key: tc.key}, tc.name)
			if tc.missing != "" {
				require.ErrorIs(t, err, errSecretUnset)
				assert.Contains(t, err.Error(), tc.missing)
				assert.Empty(t, got)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
