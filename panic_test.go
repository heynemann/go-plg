package plg_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

func TestPanicErrorMessageAndUnwrap(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pe      *plg.PanicError
		msg     string
		comment string
		is      []error
		isNot   []error
	}{
		{
			pe:  &plg.PanicError{Key: "fooer/p", Phase: plg.PhaseCall, Method: "Foo", Value: "boom"},
			msg: "plg: fooer/p: call Foo failed: boom", is: []error{plg.ErrCall},
		},
		{
			pe: &plg.PanicError{
				Key: "fooer/p", Phase: plg.PhaseCall, Method: "Spin",
				Value: fmt.Errorf("plg_call: %w", context.DeadlineExceeded),
			},
			msg: "plg: fooer/p: call Spin failed: plg_call: context deadline exceeded",
			is:  []error{plg.ErrCall, context.DeadlineExceeded}, comment: "a stop is told apart by its cause",
		},
		{
			pe:  &plg.PanicError{Key: "fooer", Phase: plg.PhaseStorage, Method: "List", Value: errStorage},
			msg: "plg: fooer: storage List failed: storage unavailable", is: []error{errStorage},
			isNot: []error{plg.ErrCall}, comment: "only calls are ErrCall",
		},
		{
			pe:    &plg.PanicError{Key: "fooer/p", Phase: plg.PhaseLoad, Value: 42},
			msg:   "plg: fooer/p: load failed: 42",
			isNot: []error{plg.ErrCall, plg.ErrLoad}, comment: "no method, a value that is not an error",
		},
	} {
		assert.Equal(t, tc.msg, tc.pe.Error())

		wrapped := fmt.Errorf("outer: %w", tc.pe)

		got, ok := errors.AsType[*plg.PanicError](wrapped)
		require.True(t, ok)
		assert.Same(t, tc.pe, got)

		for _, target := range tc.is {
			require.ErrorIs(t, wrapped, target, tc.comment)
		}

		for _, target := range tc.isNot {
			require.NotErrorIs(t, wrapped, target, tc.comment)
		}
	}
}

// panickingStorage panics in the named method; it lists nothing.
type panickingStorage string

func (s panickingStorage) Name() string {
	if s == "Name" {
		panic("name bug")
	}

	return "panicking"
}

func (s panickingStorage) List(context.Context) ([]plg.Descriptor, error) {
	if s == "List" {
		panic(errStorage)
	}

	return nil, nil
}

func (panickingStorage) Open(context.Context, *plg.Descriptor) (fs.FS, error) {
	return nil, errStorage
}

func TestPanickingStorageFailsRegister(t *testing.T) {
	t.Parallel()

	var reported []*plg.PanicError

	ctr := plg.New(plg.WithPanicHistory(1), plg.WithOnPanic(func(_ context.Context, pe *plg.PanicError) {
		reported = append(reported, pe)
	}))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	for i, tc := range []struct {
		value  any
		method string
	}{
		{"name bug", "Name"},
		{errStorage, "List"},
	} {
		name := "store" + strconv.Itoa(i)
		err := plg.Register[pinger](t.Context(), ctr, name,
			plg.WithStorage(panickingStorage(tc.method)))
		require.ErrorIs(t, err, plg.ErrLoad, tc.method)

		pe, ok := errors.AsType[*plg.PanicError](err)
		require.True(t, ok, tc.method)
		assert.Equal(t, plg.PanicError{
			Key: name, Phase: plg.PhaseStorage, Method: tc.method, Value: tc.value, Stack: pe.Stack, Time: pe.Time,
		}, *pe, tc.method)
		assert.Contains(t, string(pe.Stack), "plg_test.panickingStorage."+tc.method, "the storage's stack")

		assert.Same(t, pe, reported[len(reported)-1], "passed to the callback")
		assert.Equal(t, []*plg.PanicError{pe}, ctr.RecentPanics(), "a history of one keeps the newest")

		require.NoError(t, plg.Register[pinger](t.Context(), ctr, name, emptyStorage()),
			"the name was released")
	}

	assert.Len(t, reported, 2)
}

func TestWithPanicHistoryZeroKeepsNothing(t *testing.T) {
	t.Parallel()

	ctr := plg.New(plg.WithPanicHistory(0))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	err := plg.Register[pinger](t.Context(), ctr, "p",
		plg.WithStorage(panickingStorage("List")))
	require.ErrorIs(t, err, plg.ErrLoad)
	assert.Empty(t, ctr.RecentPanics())
}
