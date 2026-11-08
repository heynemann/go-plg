package testcase

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/offline"
)

// ValidateParam is the name of Validate's parameter, the validation input.
const ValidateParam = "in"

// Result is how one case ran.
type Result struct {
	// Case is the case's name.
	Case string
	// Failure says why the case failed; empty when it passed.
	Failure string
	// Details are the lines showing the failure: the expected and actual values (for structs, lists and maps, a
	// line per difference), and the stubs' problems.
	Details []string
	// Resources are what the case's call took; zero when it did not run (the case is malformed or its instance
	// did not start).
	Resources offline.Resources
}

// Passed reports whether the case passed.
func (r *Result) Passed() bool {
	return r.Failure == ""
}

// Run runs the cases of file, one of the test case files of the plugin whose files are files, in order, each in a
// fresh instance by runner, against contract, whose Host's Funcs runner must have been opened with ([HostFuncs]).
func Run(ctx context.Context, contract *offline.Contract, files fs.FS, runner offline.Runner, file *File) []Result {
	results := make([]Result, 0, len(file.Cases))

	for _, tc := range file.Cases {
		results = append(results, runCase(ctx, contract, files, runner, file, tc))
	}

	return results
}

// runCase runs one case.
func runCase(ctx context.Context, contract *offline.Contract, files fs.FS, runner offline.Runner, file *File,
	tc *Case,
) Result {
	result := Result{Case: tc.Name}

	if tc.err != nil {
		result.Failure = tc.err.Error()

		return result
	}

	prepared, err := prepare(contract, &decoder{files: files}, file, tc, runner.Secrets())
	if err != nil {
		result.Failure = err.Error()

		return result
	}

	outcome := runner.Run(context.WithValue(ctx, stateKey{}, prepared.state), &offline.Call{
		HTTP: file.HTTP, Args: prepared.args, Method: prepared.method,
	})

	verdict := prepared.check(&outcome)
	result.Failure, result.Details, result.Resources = verdict.failure, verdict.details, outcome.Resources

	problems := prepared.state.problems()
	if len(problems) > 0 && result.Failure == "" {
		result.Failure = "the stubs were not used as the case declares"
	}

	result.Details = append(result.Details, problems...)

	return result
}

// Invoke calls the method call (or Validate) of the plugin in a fresh instance by runner, with the arguments of
// the input file name of files: a mapping of the parameters by name, as a case's input ($ref paths relative to
// files' root); an empty name gives none. There are no stubs: runner's Host and HTTP policy answer, as they were
// opened with. It returns what the call returned, written as test cases write values, or why it failed.
func Invoke(ctx context.Context, contract *offline.Contract, files fs.FS, runner offline.Runner, call, name string,
) (string, error) {
	tc := &Case{Name: call, call: call, pos: position{file: "call " + call}}

	if name != "" {
		root, err := readYAML(files, name)
		if err != nil {
			return "", err
		}

		if root.Kind != yaml.MappingNode {
			return "", errorAt(at(name, root), "the input is a mapping of the call's parameters by name")
		}

		tc.input, tc.pos = root, at(name, root)
	}

	prepared, err := prepare(contract, &decoder{files: files}, &File{Name: name}, tc, runner.Secrets())
	if err != nil {
		return "", err
	}

	outcome := runner.Run(context.WithValue(ctx, stateKey{}, prepared.state), &offline.Call{
		Args: prepared.args, Method: prepared.method,
	})
	if outcome.Err != nil {
		return "", outcome.Err
	}

	return prepared.describeOutcome(&outcome), nil
}

// prepared is a case whose values are decoded: ready to run.
type prepared struct {
	state    *state
	expect   *expect
	want     []reflect.Value // the expected results
	call     string
	args     []reflect.Value
	results  int // how many results the method has before its error
	method   int
	validate bool // the case calls the plugin's Validate
}

// prepare decodes the values of tc, a case of file, into the contract's types, and checks that it fakes only
// secrets of declared, those plugin.yaml declares.
func prepare(contract *offline.Contract, dec *decoder, file *File, tc *Case, declared []string) (*prepared, error) {
	call := &prepared{call: tc.call, expect: tc.expect, state: &state{http: tc.http, secrets: tc.secrets}}

	for _, name := range slices.Sorted(maps.Keys(tc.secrets)) {
		if !slices.Contains(declared, name) {
			return nil, errorAt(tc.fakes, "secrets fakes %q, which plugin.yaml does not declare (secrets: [%s])",
				name, strings.Join(declared, ", "))
		}
	}

	sig, err := call.resolve(contract)
	if err != nil {
		return nil, errorAt(tc.pos, "%w", err)
	}

	call.args, err = decodeArgs(dec, file.Name, tc.input, sig.params, sig.ins, "input")
	if err != nil {
		return nil, err
	}

	if k := slices.IndexFunc(call.args, func(arg reflect.Value) bool { return !arg.IsValid() }); k >= 0 {
		return nil, errorAt(tc.pos, "input gives no %q (parameters: %s)", sig.params[k],
			strings.Join(sig.params, ", "))
	}

	err = call.decodeExpect(dec, file.Name, sig.outs)
	if err != nil {
		return nil, err
	}

	call.state.host, err = decodeHost(contract.Host, dec, file.Name, tc.host)
	if err != nil {
		return nil, err
	}

	if len(tc.http) > 0 && file.HTTP == nil {
		return nil, errorAt(tc.pos, "the case has http stubs but the file no httpPolicy: every request is refused")
	}

	return call, nil
}

// signature is a method's parameters (after the context), by name and type, and its results' types (before the
// error).
type signature struct {
	params []string
	ins    []reflect.Type
	outs   []reflect.Type
}

// resolve finds the method the case calls and returns its signature.
func (p *prepared) resolve(contract *offline.Contract) (*signature, error) {
	index := slices.IndexFunc(contract.Methods, func(m reflect.Method) bool { return m.Name == p.call })
	if index >= 0 {
		p.method = index
		fn := contract.Methods[index].Type

		return &signature{params: contract.Params[index], ins: ins(fn), outs: outs(fn)}, nil
	}

	if p.call == ValidateCall && contract.Validation != nil {
		p.method, p.validate = len(contract.Methods), true

		return &signature{params: []string{ValidateParam}, ins: []reflect.Type{contract.Validation.Input}}, nil
	}

	known := make([]string, 0, len(contract.Methods)+1)
	for _, m := range contract.Methods {
		known = append(known, m.Name)
	}

	if contract.Validation != nil {
		known = append(known, ValidateCall)
	}

	return nil, fmt.Errorf("call %q is not a method of the contract (%s)", //nolint:err113 // author's file
		p.call, strings.Join(known, ", "))
}

// outs returns the result types of fn before the error.
func outs(fn reflect.Type) []reflect.Type {
	types := make([]reflect.Type, 0, fn.NumOut()-1)
	for k := range fn.NumOut() - 1 {
		types = append(types, fn.Out(k))
	}

	return types
}

// ins returns the parameter types of fn after the context.
func ins(fn reflect.Type) []reflect.Type {
	types := make([]reflect.Type, 0, fn.NumIn()-1)
	for k := 1; k < fn.NumIn(); k++ {
		types = append(types, fn.In(k))
	}

	return types
}

// decodeArgs decodes node, a mapping of arguments by parameter name (nil: none), into values of types; what
// names it in errors. The parameters it does not give are invalid values.
func decodeArgs(dec *decoder, file string, node *yaml.Node, params []string, types []reflect.Type, what string,
) ([]reflect.Value, error) {
	args := make([]reflect.Value, len(params))

	if node != nil {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]

			k := slices.Index(params, key.Value)
			if k < 0 {
				return nil, errorAt(at(file, key), "%s: the method has no parameter %q (parameters: %s)", what,
					key.Value, strings.Join(params, ", "))
			}

			value, err := dec.decode(file, node.Content[i+1], types[k], join(what, key.Value))
			if err != nil {
				return nil, err
			}

			args[k] = value
		}
	}

	return args, nil
}

// decodeExpect decodes the expected results, of types.
func (p *prepared) decodeExpect(dec *decoder, file string, types []reflect.Type) error {
	p.results = len(types)

	want := p.expect
	if want == nil {
		return nil
	}

	switch {
	case want.issues != nil && !p.validate:
		return errorAt(want.pos, "expect.issues is for a call of Validate; %s returns no issues", p.call)
	case want.result != nil && p.validate:
		return errorAt(want.pos, "a call of Validate expects issues, not a result")
	case want.result == nil:
		return nil
	}

	var err error

	p.want, err = decodeResults(dec, file, want.result, types, "expect.result")

	return err
}

// decodeResults decodes node, the results of a method returning values of types before its error: a value with
// one result, a list of them with several; what names it in errors.
func decodeResults(dec *decoder, file string, node *yaml.Node, types []reflect.Type, what string,
) ([]reflect.Value, error) {
	switch len(types) {
	case 0:
		return nil, errorAt(at(file, node), "%s: the method returns no result besides its error", what)
	case 1:
		value, err := dec.decode(file, node, types[0], what)

		return []reflect.Value{value}, err
	}

	if node.Kind != yaml.SequenceNode || len(node.Content) != len(types) {
		return nil, errorAt(at(file, node), "%s: the method returns %d results: give a list of %d", what, len(types),
			len(types))
	}

	values := make([]reflect.Value, len(types))

	for k, item := range node.Content {
		var err error

		values[k], err = dec.decode(file, item, types[k], what+"["+strconv.Itoa(k)+"]")
		if err != nil {
			return nil, err
		}
	}

	return values, nil
}

// decodeHost decodes the Host stubs docs against host (nil: the plugin's Create takes no Host).
func decodeHost(host *offline.Host, dec *decoder, file string, docs []*hostDoc) ([]*hostStub, error) {
	if len(docs) > 0 && host == nil {
		return nil, errorAt(docs[0].pos, "the case has host stubs but the plugin's Create takes no Host (for Lua "+
			"and TypeScript plugins, name it: plg validate -host <Interface>)")
	}

	stubs := make([]*hostStub, 0, len(docs))

	for i, doc := range docs {
		what := "host[" + strconv.Itoa(i) + "]"

		method := slices.IndexFunc(host.Methods, func(m reflect.Method) bool { return m.Name == doc.method })
		if method < 0 {
			known := make([]string, len(host.Methods))
			for k, m := range host.Methods {
				known[k] = m.Name
			}

			return nil, errorAt(doc.pos, "%s: %s is not a method of the Host (%s)", what, doc.method,
				strings.Join(known, ", "))
		}

		stub, err := decodeHostStub(host, dec, file, doc, method, what)
		if err != nil {
			return nil, err
		}

		stubs = append(stubs, stub)
	}

	return stubs, nil
}

// decodeHostStub decodes the stub doc of the Host's method.
func decodeHostStub(host *offline.Host, dec *decoder, file string, doc *hostDoc, method int, what string,
) (*hostStub, error) {
	fn := host.Methods[method].Type
	params := host.Params[method]

	args, err := decodeArgs(dec, file, doc.args, params, ins(fn), what+".args")
	if err != nil {
		return nil, err
	}

	stub := &hostStub{
		method: method, times: doc.times, args: args, describe: describeCall(doc.method, params, args),
		results: make([]reflect.Value, fn.NumOut()),
	}

	types := outs(fn)
	for k, typ := range types {
		stub.results[k] = reflect.Zero(typ)
	}

	if doc.result != nil {
		values, err := decodeResults(dec, file, doc.result, types, what+".result")
		if err != nil {
			return nil, err
		}

		copy(stub.results, values)
	}

	var failure error

	if doc.err != nil {
		failure = fmt.Errorf("%s", *doc.err) //nolint:err113 // the error the case's stub returns
	}

	stub.results[len(types)] = reflect.ValueOf(&failure).Elem()

	return stub, nil
}

// verdict is why a case failed (empty: it passed) and the lines showing it.
type verdict struct {
	failure string
	details []string
}

// check compares what the call returned with the case's expectation.
func (p *prepared) check(outcome *offline.Outcome) verdict {
	want := p.expect
	if want == nil {
		want = &expect{}
	}

	switch {
	case outcome.Err != nil:
		return p.checkError(outcome, want)
	case want.err != nil:
		return verdict{p.call + " returned no error", []string{
			"expected an error containing " + strconv.Quote(*want.err), "got " + p.describeOutcome(outcome),
		}}
	case want.issues != nil:
		return checkIssues(want.issues, outcome.Issues)
	default:
		return p.checkResults(outcome.Results)
	}
}

// checkError compares the error of a call that failed with the expected one: only an error the plugin returned
// can be expected.
func (p *prepared) checkError(outcome *offline.Outcome, want *expect) verdict {
	switch {
	case want.err == nil || outcome.Returned == "":
		return verdict{p.call + " failed: " + outcome.Err.Error(), p.unexpectedError(outcome, want)}
	case strings.Contains(outcome.Returned, *want.err):
		return verdict{}
	default:
		return verdict{p.call + " returned another error than expect.error", []string{
			"expected an error containing " + strconv.Quote(*want.err), "got " + strconv.Quote(outcome.Returned),
		}}
	}
}

// unexpectedError shows a call that failed where the case expects results or issues: what it expects, and the
// error the plugin returned, if it returned one (the failure says why the call failed otherwise).
func (p *prepared) unexpectedError(outcome *offline.Outcome, want *expect) []string {
	var details []string

	switch {
	case want.issues != nil:
		details = append(details, "expected issues "+formatExpectedIssues(want.issues))
	case len(p.want) > 0:
		details = append(details, "expected "+describeResults(p.want))
	default:
		return nil // the case expects nothing the error could be compared with
	}

	if outcome.Returned != "" {
		details = append(details, "got error "+strconv.Quote(outcome.Returned))
	}

	return details
}

// describeOutcome writes what a successful call returned.
func (p *prepared) describeOutcome(outcome *offline.Outcome) string {
	if p.validate {
		return "issues " + formatIssues(outcome.Issues)
	}

	return describeResults(outcome.Results)
}

// describeResults writes a method's results, as describeOutcome does.
func describeResults(results []reflect.Value) string {
	written := make([]string, len(results))
	for k, value := range results {
		written[k] = Format(value)
	}

	switch len(written) {
	case 0:
		return "no result"
	case 1:
		return "result " + written[0]
	default:
		return "results [" + strings.Join(written, ", ") + "]"
	}
}

// checkResults compares the results with the expected ones, if any.
func (p *prepared) checkResults(got []reflect.Value) verdict {
	var details []string

	for k, want := range p.want {
		path := "result"
		if p.results > 1 {
			path += "[" + strconv.Itoa(k) + "]"
		}

		details = diff(path, want, got[k], details)
	}

	if len(details) == 0 {
		return verdict{}
	}

	return verdict{p.call + " returned other results than expect.result", details}
}

// checkIssues compares Validate's issues with the expected ones.
func checkIssues(want []issue, got []adminjson.Issue) verdict {
	var details []string

	if len(want) != len(got) {
		details = append(details, fmt.Sprintf("issues: expected %d, got %d: %s", len(want), len(got),
			formatIssues(got)))
	} else {
		for i := range want {
			details = want[i].diff("issues["+strconv.Itoa(i)+"]", &got[i], details)
		}
	}

	if len(details) == 0 {
		return verdict{}
	}

	return verdict{"Validate returned other issues than expect.issues", details}
}

// issueField is a field of an expected issue (nil: not given) and of the issue got.
type issueField struct {
	want *string
	name string
	got  string
}

// fields pairs the expected issue's fields with got's, in a case's order.
func (i *issue) fields(got *adminjson.Issue) [4]issueField {
	return [4]issueField{
		{i.severity, "severity", got.Severity},
		{i.code, "code", got.Code},
		{i.message, "message", got.Message},
		{i.field, "field", got.Field},
	}
}

// diff appends a line per field the expected issue gives that got differs in.
func (i *issue) diff(path string, got *adminjson.Issue, out []string) []string {
	for _, compared := range i.fields(got) {
		if compared.want != nil && *compared.want != compared.got {
			out = append(out, path+"."+compared.name+": expected "+strconv.Quote(*compared.want)+", got "+
				strconv.Quote(compared.got))
		}
	}

	return out
}

// formatExpectedIssues writes expected issues as a case gives them: only their fields given.
func formatExpectedIssues(issues []issue) string {
	written := make([]string, len(issues))

	for k, issue := range issues {
		var parts []string

		for _, field := range issue.fields(&adminjson.Issue{}) {
			if field.want != nil {
				parts = append(parts, field.name+": "+strconv.Quote(*field.want))
			}
		}

		written[k] = "{" + strings.Join(parts, ", ") + "}"
	}

	return "[" + strings.Join(written, ", ") + "]"
}

// formatIssues writes issues as a case would.
func formatIssues(issues []adminjson.Issue) string {
	written := make([]string, len(issues))

	for k, issue := range issues {
		parts := []string{"severity: " + strconv.Quote(issue.Severity), "code: " + strconv.Quote(issue.Code)}
		if issue.Message != "" {
			parts = append(parts, "message: "+strconv.Quote(issue.Message))
		}

		if issue.Field != "" {
			parts = append(parts, "field: "+strconv.Quote(issue.Field))
		}

		written[k] = "{" + strings.Join(parts, ", ") + "}"
	}

	return "[" + strings.Join(written, ", ") + "]"
}
