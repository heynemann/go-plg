package testcase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/heynemann/go-plg/internal/offline"
)

var (
	// errNoHostStub fails a Host call no stub of the running case matches.
	errNoHostStub = errors.New("plg validate: no host stub of the test case matches")
	// errNoHTTPStub fails a request no stub of the running case matches.
	errNoHTTPStub = errors.New("plg validate: no http stub of the test case matches")
	// errNoSecretFake fails a read of a secret the running case does not fake.
	errNoSecretFake = errors.New("plg validate: the test case fakes no such secret")
	// errFailingSecret fails a read of a secret the running case fakes as failing (null).
	errFailingSecret = errors.New("plg validate: the test case fakes this secret's read failing")
	// errNoCase fails a Host call, request or secret read made outside a test case.
	errNoCase = errors.New("plg validate: no test case is running")
)

// stateKey is the context key of the running case's [state].
type stateKey struct{}

// state is a running case's stubs and fakes and what happened to them. The plugin's Host calls, requests and secret
// reads reach it from the goroutine running the call, while Run waits for it, so its counts are atomic.
type state struct {
	unmatched atomic.Pointer[[]string] // the Host calls, requests and secret reads no stub matched, in order
	secrets   map[string]*string       // the secrets' fakes; nil fails the read
	host      []*hostStub
	http      []*httpStub
}

// stateOf returns the case running under ctx.
func stateOf(ctx context.Context) (*state, error) {
	if s, ok := ctx.Value(stateKey{}).(*state); ok {
		return s, nil
	}

	return nil, errNoCase
}

// Secret answers a read of the secret name, for [offline.Options]' Secrets, from the fakes of the case running under
// ctx: the value; or an error, recording a problem that fails the case, for a secret it does not fake (or a read
// outside a case); or [errFailingSecret] for a secret it fakes as failing (null).
func Secret(ctx context.Context, name string) (string, error) {
	running, err := stateOf(ctx)
	if err != nil {
		return "", err
	}

	fake, ok := running.secrets[name]

	switch {
	case !ok:
		running.miss("secret " + name + " was read, but the case fakes no value for it under secrets:")

		return "", fmt.Errorf("%w: %s", errNoSecretFake, name)
	case fake == nil:
		return "", errFailingSecret
	default:
		return *fake, nil
	}
}

// miss records a Host call or request no stub matched.
func (s *state) miss(what string) {
	for {
		old := s.unmatched.Load()

		var next []string

		if old != nil {
			next = append(next, *old...)
		}

		next = append(next, what)

		if s.unmatched.CompareAndSwap(old, &next) {
			return
		}
	}
}

// problems lists what went wrong with the stubs: Host calls and requests no stub matched, then stubs with times
// that answered another number of calls.
func (s *state) problems() []string {
	var problems []string

	if unmatched := s.unmatched.Load(); unmatched != nil {
		problems = append(problems, *unmatched...)
	}

	for _, stub := range s.host {
		problems = stub.calls.check(problems, "host stub "+stub.describe, stub.times)
	}

	for _, stub := range s.http {
		problems = stub.calls.check(problems, "http stub "+stub.method+" "+stub.url, stub.times)
	}

	return problems
}

// counter counts the calls a stub answered.
type counter struct {
	n atomic.Int64
}

// check appends a problem to problems when the count is not times (-1: any count).
func (c *counter) check(problems []string, what string, times int) []string {
	if n := c.n.Load(); times >= 0 && n != int64(times) {
		return append(problems, fmt.Sprintf("%s answered %d %s, expected %d", what, n, calls(n), times))
	}

	return problems
}

func calls(n int64) string {
	if n == 1 {
		return "call"
	}

	return "calls"
}

// hostStub is a Host stub with its values decoded.
type hostStub struct {
	describe string          // the call it answers, for messages
	args     []reflect.Value // the arguments it matches, by parameter (after the context); invalid: any
	results  []reflect.Value // what it returns: the results, then the error
	calls    counter
	times    int
	method   int
}

// HostFuncs returns the functions answering the methods of host, in its Methods' order, for [offline.Host]'s
// Funcs: each answers from the stubs of the case running under the context it is called with, and fails a call
// no stub matches (or made outside a case).
func HostFuncs(host *offline.Host) []reflect.Value {
	funcs := make([]reflect.Value, len(host.Methods))

	for i, method := range host.Methods {
		typ := method.Type

		funcs[i] = reflect.MakeFunc(typ, func(args []reflect.Value) []reflect.Value {
			ctx, _ := reflect.TypeAssert[context.Context](args[0]) // the host hands every Host call a context

			s, err := stateOf(ctx)
			if err == nil {
				return s.answer(host, i, args[1:])
			}

			return failed(typ, err)
		})
	}

	return funcs
}

// answer answers a call of the Host method with args (after the context) from the first stub matching it.
func (s *state) answer(host *offline.Host, method int, args []reflect.Value) []reflect.Value {
	for _, stub := range s.host {
		if stub.method == method && stub.matches(args) {
			stub.calls.n.Add(1)

			return stub.results
		}
	}

	call := describeCall(host.Methods[method].Name, host.Params[method], args)
	s.miss("Host call " + call + " matched no host stub")

	return failed(host.Methods[method].Type, fmt.Errorf("%w %s", errNoHostStub, call))
}

// matches reports whether args (after the context) are the stub's.
func (h *hostStub) matches(args []reflect.Value) bool {
	for i, want := range h.args {
		if want.IsValid() && len(diff("", want, args[i], nil)) > 0 {
			return false
		}
	}

	return true
}

// describeCall writes a call: Tier(user: "alice").
func describeCall(method string, params []string, args []reflect.Value) string {
	written := make([]string, 0, len(args))

	for i, arg := range args {
		if arg.IsValid() {
			written = append(written, params[i]+": "+Format(arg))
		}
	}

	return method + "(" + strings.Join(written, ", ") + ")"
}

// failed returns zero results of fn, a Host method's type, and err.
func failed(fn reflect.Type, err error) []reflect.Value {
	out := make([]reflect.Value, fn.NumOut())
	for k := range out {
		out[k] = reflect.Zero(fn.Out(k))
	}

	out[len(out)-1] = reflect.ValueOf(&err).Elem()

	return out
}

// transport answers the requests of the case running under each request's context from its HTTP stubs.
type transport struct{}

// RoundTrip implements [http.RoundTripper].
func (transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		req.Body.Close() //nolint:errcheck,gosec // a RoundTripper closes the request's body; the stubs never read it
	}

	running, err := stateOf(req.Context())
	if err != nil {
		return nil, err
	}

	target := req.URL.String()

	for _, stub := range running.http {
		if stub.method == req.Method && stub.url == target {
			stub.calls.n.Add(1)

			return &http.Response{
				Status:        strconv.Itoa(stub.status) + " " + http.StatusText(stub.status),
				StatusCode:    stub.status,
				Proto:         "HTTP/1.1",
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        stub.header.Clone(),
				Body:          io.NopCloser(strings.NewReader(stub.body)),
				ContentLength: int64(len(stub.body)),
				Request:       req,
			}, nil
		}
	}

	running.miss("HTTP request " + req.Method + " " + target + " matched no http stub")

	return nil, fmt.Errorf("%w %s %s", errNoHTTPStub, req.Method, target)
}
