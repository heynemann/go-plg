package goguest_test

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/goguest"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
	"github.com/heynemann/go-plg/plugin"
)

// validatorPlugin is a plugin with the given declarations besides Fooer's (unimplemented: they panic), whose
// Create returns instance; it imports what Validate methods use.
func validatorPlugin(instance, decls string) map[string]string {
	return map[string]string{"p.go": `package p

import (
	"context"
	"errors"
	"fmt"

	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/plugin"
)

var (
	_, _ = errors.New, fmt.Sprint
	_    context.Context
	_    plugin.Issue
)

type validator struct{ testcontract.Fooer }

func (*validator) Count(context.Context) (int, error) { return 7, nil }

func Create() (testcontract.Fooer, error) { return ` + instance + `, nil }

` + decls}
}

// loadValidating loads the plugin of files with a dispatcher binding Validate(context.Context, testcontract.Check).
func loadValidating(t *testing.T, files map[string]string, output io.Writer) *goguest.Guest {
	t.Helper()

	dispatcher, err := goabi.RenderDispatcher(fooerType(), nil, reflect.TypeFor[testcontract.Check]())
	require.NoError(t, err)

	payload := goabi.InitPayload{
		GoPath: gopathWith(t, files, string(dispatcher)), Dispatch: goabi.DispatchImportPath, Methods: methodNames(),
		Validate: true,
	}
	encoded, err := payload.Append(nil)
	require.NoError(t, err)

	guest := goguest.New(output, io.Discard, nil) // yaegi writes a panic's trace to stderr
	res := parse(t, guest.Init(send(guest, encoded)))
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	return guest
}

// validate calls the guest's Validate, the method after Fooer's.
func validate(t *testing.T, guest *goguest.Guest, in testcontract.Check) result {
	t.Helper()

	return parse(t, guest.Call(uint32(len(methodNames())), send(guest, encode(t, in)))) //nolint:gosec // G115: few
}

// TestValidateIsBoundOnlyWithItsExactSignature declares Validate methods on a plugin instance and calls the
// guest's Validate: only Validate(context.Context, testcontract.Check) ([]plugin.Issue, error), whatever its
// receiver, is called (its issues, error or panic answered); any other signature, a lookalike type included, is
// answered as no Validate (StatusLoad), without running the plugin's method. A Validate with one parameter more in
// front, which yaegi's interface assertion lets through, is one of them.
//
//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestValidateIsBoundOnlyWithItsExactSignature(t *testing.T) {
	const ran = "ran\n" // what the wrong signatures print when they run

	tests := []struct {
		name     string
		decls    string
		instance string // what Create returns; &validator{} when empty
		msg      string // of the answer, for errors
		issues   []plugin.Issue
		status   wire.Status
	}{
		{
			name: "pointer receiver",
			decls: `func (*validator) Validate(_ context.Context, in testcontract.Check) ([]plugin.Issue, error) {
	return []plugin.Issue{{Severity: plugin.Warning, Code: "c." + in.Mode, Message: in.URL, Field: "f"}}, nil
}`,
			issues: []plugin.Issue{{Severity: plugin.Warning, Code: "c.mode", Message: "url", Field: "f"}},
			status: wire.StatusOK,
		},
		{
			name: "value receiver, no issues",
			decls: `func (validator) Validate(context.Context, testcontract.Check) ([]plugin.Issue, error) {
	return nil, nil
}`,
			status: wire.StatusOK,
		},
		{
			name: "promoted from an embedded type",
			decls: `type base struct{}

func (base) Validate(_ context.Context, in testcontract.Check) ([]plugin.Issue, error) {
	return []plugin.Issue{{Severity: "fatal", Code: "embedded"}}, nil
}

type promoting struct {
	base
	testcontract.Fooer
}`,
			instance: "promoting{}",
			issues:   []plugin.Issue{{Severity: "fatal", Code: "embedded"}},
			status:   wire.StatusOK,
		},
		{
			name: "returning an error",
			decls: `func (*validator) Validate(context.Context, testcontract.Check) ([]plugin.Issue, error) {
	return nil, errors.New("cannot validate")
}`,
			msg:    "cannot validate",
			status: wire.StatusError,
		},
		{
			name: "panicking",
			decls: `func (*validator) Validate(context.Context, testcontract.Check) ([]plugin.Issue, error) {
	panic("validate panicked")
}`,
			msg:    "validate panicked",
			status: wire.StatusPanic,
		},
		{name: "absent", status: wire.StatusLoad},
		{
			name: "on another type only",
			decls: `type other struct{}

func (other) Validate(context.Context, testcontract.Check) ([]plugin.Issue, error) { return nil, nil }`,
			status: wire.StatusLoad,
		},
		{
			name: "one parameter more in front",
			decls: `func (*validator) Validate(n int, _ context.Context, in testcontract.Check) ([]plugin.Issue, error) {
	fmt.Print("ran\n")
	return []plugin.Issue{{Code: "leading"}}, nil
}`,
			status: wire.StatusLoad,
		},
		{
			name: "one parameter more at the end",
			decls: `func (*validator) Validate(_ context.Context, in testcontract.Check, n int) ([]plugin.Issue, error) {
	fmt.Print("ran\n")
	return nil, nil
}`,
			status: wire.StatusLoad,
		},
		{
			name: "no context",
			decls: `func (*validator) Validate(in testcontract.Check) ([]plugin.Issue, error) {
	fmt.Print("ran\n")
	return nil, nil
}`,
			status: wire.StatusLoad,
		},
		{
			name: "another input type",
			decls: `func (*validator) Validate(_ context.Context, in testcontract.Point) ([]plugin.Issue, error) {
	fmt.Print("ran\n")
	return nil, nil
}`,
			status: wire.StatusLoad,
		},
		{
			// yaegi compares interpreted struct types by their fields, so a type the plugin declares with the
			// input's fields is the input type to it: bound, and called with the input.
			name: "a lookalike input type",
			decls: `type Check struct{ Mode, URL string }

func (*validator) Validate(_ context.Context, in Check) ([]plugin.Issue, error) {
	return []plugin.Issue{{Code: in.Mode}}, nil
}`,
			issues: []plugin.Issue{{Code: "mode"}},
			status: wire.StatusOK,
		},
		{
			name: "a lookalike issue type",
			decls: `type Issue struct{ Severity, Code, Message, Field string }

func (*validator) Validate(context.Context, testcontract.Check) ([]Issue, error) {
	fmt.Print("ran\n")
	return nil, nil
}`,
			status: wire.StatusLoad,
		},
		{
			name: "issue pointers",
			decls: `func (*validator) Validate(context.Context, testcontract.Check) ([]*plugin.Issue, error) {
	fmt.Print("ran\n")
	return nil, nil
}`,
			status: wire.StatusLoad,
		},
		{
			name: "only an error",
			decls: `func (*validator) Validate(context.Context, testcontract.Check) error {
	fmt.Print("ran\n")
	return nil
}`,
			status: wire.StatusLoad,
		},
		{
			name:   "no parameters, no results",
			decls:  `func (*validator) Validate() { fmt.Print("ran\n") }`,
			status: wire.StatusLoad,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer

			instance := tc.instance
			if instance == "" {
				instance = "&validator{}"
			}

			guest := loadValidating(t, validatorPlugin(instance, tc.decls), &output)
			res := validate(t, guest, testcontract.Check{Mode: "mode", URL: "url"})
			require.Equal(t, tc.status, res.status, res.msg)
			assert.Contains(t, res.msg, tc.msg)
			assert.NotContains(t, output.String(), ran, "a Validate of another signature never runs")

			if tc.status == wire.StatusOK {
				assert.Equal(t, tc.issues, decode(t, res.body, []plugin.Issue{})[0])
			}

			if tc.status == wire.StatusLoad {
				res = call(t, guest, "Count")
				require.Equal(t, wire.StatusOK, res.status, "the instance serves on without a Validate: %s", res.msg)
				assert.Equal(t, 7, decode(t, res.body, 0)[0])
			}
		})
	}
}

// TestValidateNeedsTheValidatingDispatcher calls the method after Fooer's on a guest loaded without validation:
// there is none.
//
//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestValidateNeedsTheValidatingDispatcher(t *testing.T) {
	guest, res := load(
		t,
		validatorPlugin(
			"&validator{}",
			`func (*validator) Validate(context.Context, testcontract.Check) ([]plugin.Issue, error) {
	return nil, nil
}`,
		),
	)
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	res = validate(t, guest, testcontract.Check{})
	assert.Equal(t, wire.StatusABI, res.status)
	assert.Contains(t, res.msg, "unknown method index")
}
