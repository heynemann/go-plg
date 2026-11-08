// Package testcase reads and runs a plugin's test cases: the tests/*.yaml files beside its plugin.yaml, written
// once for every guest language. Each case calls one method of the contract (or the plugin's Validate) with
// inputs by parameter name, in a fresh instance of the real guest (see [offline.Runner]), with stubs answering
// the Host's methods and the plugin's HTTP requests, and checks what the call returned against its expectation.
// Values are the contract's data types written as YAML, struct fields by the names other guest languages see
// (their json tag, else their Go name), and decoded strictly: a field the type does not have, a value of another
// kind or out of the type's range is an error of the case, never a silent zero. Anywhere a value goes, a mapping
// {$ref: <path>} takes it from another YAML file of the plugin (a fixture), relative to the plugin's directory.
//
// A file:
//
//	httpPolicy:                      # optional: the HTTP policy the cases run under, as plg.HTTPPolicy;
//	  allowedHosts: [api.example.com] #   without it every request is refused, as without plg.WithHTTP
//	  maxRequestsPerCall: 4          #   also timeout (a duration), maxRequestBytes, maxResponseBytes
//	cases:
//	  - name: gold members only      # unique in the file
//	    call: IsMember               # a method of the contract, or Validate
//	    input:                       # every parameter after the context, by name (Validate's is "in")
//	      user: {$ref: fixtures/alice.yaml}
//	    host:                        # Host stubs, the first matching one answers
//	      - method: Tier
//	        args: {user: alice}      # optional: the parameters it matches, by name; the others match anything
//	        result: gold             # its results before the error (a list when there are several), or
//	        # error: no such user    #   the error it returns
//	        times: 1                 # optional: how many calls the case must make that the stub answers
//	    http:                        # HTTP stubs, matched by method and URL
//	      - method: GET              # default GET
//	        url: https://api.example.com/users/alice
//	        status: 200              # default 200
//	        headers: {Content-Type: application/json}
//	        body: '{"tier":"gold"}'
//	        times: 1
//	    secrets:                     # fakes of the secrets plugin.yaml declares, by name: each read gets the
//	      crm_token: t0k3n           #   value, or fails as a failing resolver's would (null); a name it does
//	      backup_token: null         #   not give fails the read and the case
//	    expect:                      # at most one of: the results (a list when there are several), the
//	      result: true               #   substring of the error the plugin returns, or Validate's issues
//	      # error: not found
//	      # issues: [{severity: warning, code: rule.slow}]  # fields left out match anything
//
// A case fails when its call fails otherwise than expected (an error it did not expect, a panic, a trap, a
// timeout), returns other results or issues than it expects, makes a Host call or HTTP request no stub matches, or
// reads a secret it does not fake (the call itself gets an error then), or a stub with times answers another
// number of calls. Requests the policy refuses never reach the stubs: the plugin gets the refusal, as it would in a
// service.
package testcase

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/offline"
	"github.com/heynemann/go-plg/internal/strictyaml"
)

// Glob matches a plugin's test case files.
const Glob = "tests/*.yaml"

// ValidateCall is the call of a case that calls the plugin's Validate with the contract's validation input.
const ValidateCall = "Validate"

// maxFileSize bounds a test case file and a fixture.
const maxFileSize = 1 << 20

// File is one test case file.
type File struct {
	// Err is why the file cannot run: it cannot be read or parsed, or its httpPolicy is malformed.
	Err error
	// HTTP is the policy its cases run under, answered by their HTTP stubs; nil refuses every request.
	HTTP *offline.HTTP
	// Name is its path in the plugin's directory.
	Name  string
	Cases []*Case
}

// Case is one test case.
type Case struct {
	err     error      // why the case is malformed
	input   *yaml.Node // a mapping; nil without input
	expect  *expect
	secrets map[string]*string // the fakes by name; nil fails the read
	Name    string
	call    string
	host    []*hostDoc
	http    []*httpStub
	pos     position
	fakes   position // where secrets is
}

// hostDoc is a Host stub as written: its values decode once the Host's types are known.
type hostDoc struct {
	args   *yaml.Node // a mapping; nil matches any arguments
	result *yaml.Node // nil: zero results
	err    *string
	method string
	pos    position
	times  int // -1: any number of calls
}

// expect is a case's expectation; at most one of its fields is set.
type expect struct {
	result *yaml.Node
	err    *string
	issues []issue
	pos    position
}

// issue is an expected issue: the fields it gives (nil matches anything).
type issue struct {
	severity, code, message, field *string
}

// position is where a node is in a file; without a line, where a value came from.
type position struct {
	file      string
	line, col int
}

func (p position) String() string {
	if p.line == 0 {
		return p.file
	}

	return p.file + ":" + strconv.Itoa(p.line) + ":" + strconv.Itoa(p.col)
}

// at returns where node is in file.
func at(file string, node *yaml.Node) position {
	return position{file: file, line: node.Line, col: node.Column}
}

// errorAt returns an error at pos.
func errorAt(pos position, format string, args ...any) error {
	return fmt.Errorf("%s: %w", pos, fmt.Errorf(format, args...)) //nolint:err113 // a message about the author's file
}

// Load reads the test case files of the plugin whose files are files, in name order. A file that cannot be read or
// parsed is returned with its Err set.
func Load(files fs.FS) []*File {
	names, _ := fs.Glob(files, Glob) //nolint:errcheck // the only error is a malformed pattern, and Glob's is not

	loaded := make([]*File, 0, len(names))
	for _, name := range names {
		loaded = append(loaded, loadFile(files, name))
	}

	return loaded
}

// loadFile reads and parses the test case file name.
func loadFile(files fs.FS, name string) *File {
	file := &File{Name: name}

	root, err := readYAML(files, name)
	if err == nil && root.Kind != yaml.MappingNode {
		err = errorAt(at(name, root), "a test case file is a mapping with cases (and an httpPolicy)")
	}

	if err == nil {
		err = file.parse(root)
	}

	file.Err = err

	return file
}

// readYAML reads the YAML file name of files, at most maxFileSize bytes, and returns its root node.
func readYAML(files fs.FS, name string) (*yaml.Node, error) {
	opened, err := files.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // fs errors name the file
	}

	defer opened.Close() //nolint:errcheck // read only

	data, err := io.ReadAll(io.LimitReader(opened, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}

	if len(data) > maxFileSize {
		return nil, fmt.Errorf("%s: %w", name, errTooLarge)
	}

	root, err := strictyaml.Parse(data, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	return root, nil
}

var errTooLarge = fmt.Errorf("larger than %d bytes", maxFileSize)

// parse parses the file's root mapping.
func (f *File) parse(root *yaml.Node) error {
	keys, err := mapping(f.Name, root, "a test case file", "cases", "httpPolicy")
	if err != nil {
		return err
	}

	if node := keys["httpPolicy"]; node != nil {
		f.HTTP, err = parsePolicy(f.Name, node)
		if err != nil {
			return err
		}
	}

	node := keys["cases"]
	if node == nil {
		return errorAt(at(f.Name, root), "the file has no cases")
	}

	if node.Kind != yaml.SequenceNode {
		return errorAt(at(f.Name, node), "cases is a list of test cases")
	}

	names := map[string]bool{}

	for _, item := range node.Content {
		tc := parseCase(f.Name, item)
		if tc.err == nil && names[tc.Name] {
			tc.err = errorAt(tc.pos, "another case of the file is named %q", tc.Name)
		}

		names[tc.Name] = true
		f.Cases = append(f.Cases, tc)
	}

	return nil
}

// parsePolicy parses a file's httpPolicy.
func parsePolicy(file string, node *yaml.Node) (*offline.HTTP, error) {
	keys, err := mapping(file, node, "httpPolicy", "allowedHosts", "maxRequestBytes", "maxRequestsPerCall",
		"maxResponseBytes", "timeout")
	if err != nil {
		return nil, err
	}

	policy := &offline.HTTP{Transport: transport{}}

	err = firstError(
		func() error { return parseAllowed(file, node, keys["allowedHosts"], policy) },
		func() error { return setCount(file, keys, "maxRequestBytes", &policy.MaxRequestBytes) },
		func() error { return setCount(file, keys, "maxRequestsPerCall", &policy.MaxRequestsPerCall) },
		func() error { return setCount(file, keys, "maxResponseBytes", &policy.MaxResponseBytes) },
		func() error { return setDuration(file, keys, "timeout", &policy.Timeout) },
	)
	if err != nil {
		return nil, err
	}

	return policy, nil
}

// parseAllowed parses the allowedHosts node of the policy node into policy.
func parseAllowed(file string, node, hosts *yaml.Node, policy *offline.HTTP) error {
	if hosts == nil || hosts.Kind != yaml.SequenceNode || len(hosts.Content) == 0 {
		return errorAt(at(file, node), "httpPolicy needs allowedHosts, a list of the hosts the plugin may reach")
	}

	for _, host := range hosts.Content {
		name, err := text(file, host, "an allowed host")
		if err != nil {
			return err
		}

		policy.AllowedHosts = append(policy.AllowedHosts, name)
	}

	return nil
}

// firstError runs steps in order until one fails, returning its error.
func firstError(steps ...func() error) error {
	for _, step := range steps {
		err := step()
		if err != nil {
			return err
		}
	}

	return nil
}

// parseCase parses one case; a malformed case has its err set.
func parseCase(file string, node *yaml.Node) *Case {
	tc := &Case{pos: at(file, node)}

	keys, err := mapping(file, node, "a test case", "call", "expect", "host", "http", "input", "name", "secrets")
	if err == nil {
		err = tc.parse(file, keys)
	}

	tc.err = err

	return tc
}

func (tc *Case) parse(file string, keys map[string]*yaml.Node) error {
	var err error

	tc.Name, err = required(file, tc.pos, keys, "name")
	if err != nil {
		return err
	}

	tc.call, err = required(file, tc.pos, keys, "call")
	if err != nil {
		return err
	}

	if node := keys["input"]; node != nil {
		if node.Kind != yaml.MappingNode {
			return errorAt(at(file, node), "input is a mapping of the call's parameters by name")
		}

		tc.input = node
	}

	tc.host, err = list(file, keys["host"], "host", parseHost)
	if err != nil {
		return err
	}

	tc.http, err = list(file, keys["http"], "http", parseHTTP)
	if err != nil {
		return err
	}

	if node := keys["secrets"]; node != nil {
		tc.fakes = at(file, node)

		tc.secrets, err = parseSecrets(file, node)
		if err != nil {
			return err
		}
	}

	if node := keys["expect"]; node != nil {
		tc.expect, err = parseExpect(file, node)
	}

	return err
}

// parseSecrets parses a case's secret fakes: a mapping of names to strings, or null for a failing read.
func parseSecrets(file string, node *yaml.Node) (map[string]*string, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errorAt(at(file, node), "secrets is a mapping of secret names to values, not %s", describe(node))
	}

	fakes := make(map[string]*string, len(node.Content)/2)

	for i := 0; i < len(node.Content); i += 2 {
		name, value := node.Content[i].Value, node.Content[i+1]

		switch {
		case value.Kind == yaml.ScalarNode && value.Tag == nullTag:
			fakes[name] = nil
		case value.Kind == yaml.ScalarNode && value.Tag == strTag:
			fakes[name] = &value.Value
		default:
			return nil, errorAt(at(file, value), "secrets: %s is a string, or null for a read that fails, not %s",
				name, describe(value))
		}
	}

	return fakes, nil
}

// parseHost parses a Host stub.
func parseHost(file string, node *yaml.Node) (*hostDoc, error) {
	keys, err := mapping(file, node, "a host stub", "args", "error", "method", "result", "times")
	if err != nil {
		return nil, err
	}

	stub := &hostDoc{pos: at(file, node), args: keys["args"], result: keys["result"], times: -1}

	stub.method, err = required(file, stub.pos, keys, "method")
	if err != nil {
		return nil, err
	}

	if stub.args != nil && stub.args.Kind != yaml.MappingNode {
		return nil, errorAt(at(file, stub.args), "args is a mapping of the method's parameters by name")
	}

	stub.err, err = optionalText(file, keys["error"], "error")
	if err != nil {
		return nil, err
	}

	if stub.err != nil && stub.result != nil {
		return nil, errorAt(stub.pos, "a host stub returns a result or an error, not both")
	}

	err = setCount(file, keys, "times", &stub.times)
	if err != nil {
		return nil, err
	}

	return stub, nil
}

// httpStub is an HTTP stub: the requests it answers (method and URL) and its response.
type httpStub struct {
	header http.Header
	method string
	url    string // normalized as the request's URL is
	body   string
	status int
	times  int // -1: any number of requests
	calls  counter
}

// parseHTTP parses an HTTP stub.
func parseHTTP(file string, node *yaml.Node) (*httpStub, error) {
	keys, err := mapping(file, node, "an http stub", "body", "headers", "method", "status", "times", "url")
	if err != nil {
		return nil, err
	}

	stub := &httpStub{method: http.MethodGet, status: http.StatusOK, times: -1, header: http.Header{}}

	err = firstError(
		func() error { return stub.parseRequest(file, at(file, node), keys) },
		func() error { return stub.parseStatus(file, keys["status"]) },
		func() error { return parseHeaders(file, keys["headers"], stub.header) },
		func() error { return setText(file, keys, "body", &stub.body) },
		func() error { return setCount(file, keys, "times", &stub.times) },
	)
	if err != nil {
		return nil, err
	}

	return stub, nil
}

// parseRequest parses the method and URL of the requests the stub, at pos, answers.
func (h *httpStub) parseRequest(file string, pos position, keys map[string]*yaml.Node) error {
	raw, err := required(file, pos, keys, "url")
	if err != nil {
		return err
	}

	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return errorAt(at(file, keys["url"]), "url %q is not an absolute http or https URL", raw)
	}

	h.url = target.String()

	err = setText(file, keys, "method", &h.method)
	h.method = strings.ToUpper(h.method)

	return err
}

// parseStatus parses the stub's status code (nil: 200).
func (h *httpStub) parseStatus(file string, node *yaml.Node) error {
	if node == nil {
		return nil
	}

	status, err := count(file, node, "status")
	if err == nil && (status < 100 || status > 599) {
		err = errorAt(at(file, node), "status %d is not an HTTP status code", status)
	}

	h.status = status

	return err
}

// parseHeaders parses an HTTP stub's headers (nil: none) into header.
func parseHeaders(file string, node *yaml.Node, header http.Header) error {
	if node == nil {
		return nil
	}

	if node.Kind != yaml.MappingNode {
		return errorAt(at(file, node), "headers is a mapping of header names to values")
	}

	for i := 0; i < len(node.Content); i += 2 {
		value, err := text(file, node.Content[i+1], "a header value")
		if err != nil {
			return err
		}

		header.Add(node.Content[i].Value, value)
	}

	return nil
}

// parseExpect parses a case's expectation.
func parseExpect(file string, node *yaml.Node) (*expect, error) {
	keys, err := mapping(file, node, "expect", "error", "issues", "result")
	if err != nil {
		return nil, err
	}

	if len(keys) > 1 {
		return nil, errorAt(at(file, node), "expect gives one of result, error and issues")
	}

	want := &expect{pos: at(file, node), result: keys["result"]}

	want.err, err = optionalText(file, keys["error"], "error")
	if err != nil {
		return nil, err
	}

	if node := keys["issues"]; node != nil {
		want.issues, err = list(file, node, "issues", parseIssue)
		if want.issues == nil {
			want.issues = []issue{}
		}
	}

	return want, err
}

// parseIssue parses an expected issue.
func parseIssue(file string, node *yaml.Node) (issue, error) {
	keys, err := mapping(file, node, "an issue", "code", "field", "message", "severity")
	if err != nil {
		return issue{}, err
	}

	var want issue

	for _, f := range []struct {
		field **string
		key   string
	}{{&want.code, "code"}, {&want.field, "field"}, {&want.message, "message"}, {&want.severity, "severity"}} {
		*f.field, err = optionalText(file, keys[f.key], f.key)
		if err != nil {
			return issue{}, err
		}
	}

	return want, nil
}

// mapping returns the values of node, a mapping, by key; what names it in errors, and a key not in allowed is one.
func mapping(file string, node *yaml.Node, what string, allowed ...string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errorAt(at(file, node), "%s is a mapping", what)
	}

	keys := make(map[string]*yaml.Node, len(node.Content)/2)

	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !slices.Contains(allowed, key) {
			return nil, errorAt(at(file, node.Content[i]), "%s has no key %q (keys: %s)", what, key,
				strings.Join(allowed, ", "))
		}

		keys[key] = node.Content[i+1]
	}

	return keys, nil
}

// list parses node, a sequence (nil: none), item by item.
func list[T any](file string, node *yaml.Node, what string, item func(string, *yaml.Node) (T, error)) ([]T, error) {
	if node == nil {
		return nil, nil
	}

	if node.Kind != yaml.SequenceNode {
		return nil, errorAt(at(file, node), "%s is a list", what)
	}

	items := make([]T, 0, len(node.Content))

	for _, child := range node.Content {
		parsed, err := item(file, child)
		if err != nil {
			return nil, err
		}

		items = append(items, parsed)
	}

	return items, nil
}

// required returns the string value of key, which keys must have.
func required(file string, pos position, keys map[string]*yaml.Node, key string) (string, error) {
	node := keys[key]
	if node == nil {
		return "", errorAt(pos, "%s is required", key)
	}

	value, err := text(file, node, key)
	if err == nil && value == "" {
		err = errorAt(at(file, node), "%s is empty", key)
	}

	return value, err
}

// optionalText returns the string value of node; nil when node is nil.
func optionalText(file string, node *yaml.Node, what string) (*string, error) {
	if node == nil {
		return nil, nil //nolint:nilnil // an absent value is not an error
	}

	value, err := text(file, node, what)
	if err != nil {
		return nil, err
	}

	return &value, nil
}

// setText sets *value to the string of key, when keys has it.
func setText(file string, keys map[string]*yaml.Node, key string, value *string) error {
	text, err := optionalText(file, keys[key], key)
	if text != nil {
		*value = *text
	}

	return err
}

// setCount sets *value to the non-negative integer of key, when keys has it.
func setCount(file string, keys map[string]*yaml.Node, key string, value *int) error {
	if keys[key] == nil {
		return nil
	}

	n, err := count(file, keys[key], key)
	*value = n

	return err
}

// setDuration sets *value to the positive duration of key, when keys has it.
func setDuration(file string, keys map[string]*yaml.Node, key string, value *time.Duration) error {
	if keys[key] == nil {
		return nil
	}

	d, err := duration(file, keys[key], key)
	*value = d

	return err
}

// text returns the value of node, a string.
func text(file string, node *yaml.Node, what string) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != strTag {
		return "", errorAt(at(file, node), "%s is a string, not %s", what, describe(node))
	}

	return node.Value, nil
}

// count returns the value of node, a non-negative integer.
func count(file string, node *yaml.Node, what string) (int, error) {
	var n int

	if node.Kind != yaml.ScalarNode || node.Tag != intTag || node.Decode(&n) != nil || n < 0 {
		return 0, errorAt(at(file, node), "%s is a non-negative integer, not %s", what, describe(node))
	}

	return n, nil
}

// duration returns the value of node, a positive duration ("1.5s").
func duration(file string, node *yaml.Node, what string) (time.Duration, error) {
	value, err := text(file, node, what)
	if err != nil {
		return 0, err
	}

	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, errorAt(at(file, node), "%s is a positive duration such as 1.5s, not %q", what, value)
	}

	return d, nil
}

// YAML's core tags.
const (
	strTag   = "!!str"
	intTag   = "!!int"
	floatTag = "!!float"
	boolTag  = "!!bool"
	nullTag  = "!!null"
	timeTag  = "!!timestamp"
)

// aMapping is what a mapping node is, in errors.
const aMapping = "a mapping"

// describe names what node is, for errors.
func describe(node *yaml.Node) string {
	switch node.Kind { //nolint:exhaustive // the other kinds are scalars here: documents and aliases are refused
	case yaml.MappingNode:
		return aMapping
	case yaml.SequenceNode:
		return "a list"
	default:
		return strings.TrimPrefix(node.Tag, "!!") + " " + strconv.Quote(node.Value)
	}
}

// cleanRef returns ref, a fixture's path relative to the plugin's directory, cleaned; it fails for a path that
// leaves the directory.
func cleanRef(ref string) (string, error) {
	cleaned := path.Clean(ref)
	if !fs.ValidPath(cleaned) || cleaned == "." {
		return "", fmt.Errorf("$ref %q is not a path inside the plugin's directory", ref) //nolint:err113 // author's
	}

	return cleaned, nil
}
