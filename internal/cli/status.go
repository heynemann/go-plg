package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg/internal/adminjson"
)

// adminTimeout bounds each request to an admin handler.
const adminTimeout = 30 * time.Second

// maxErrorAnswer is how much of an error answer's body plg reads to explain it.
const maxErrorAnswer = 4 << 10

// The output formats of status and events.
const (
	formatTable = "table"
	formatJSON  = "json"
)

var errAnswer = errors.New("the admin handler answered")

// runStatus runs `plg status`.
func runStatus(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	common := newAdminFlags(flags)
	team := flags.String("team", "", "only the plugins whose owner team is this one")

	keys, res, ok := parseMixed("status", flags, args)
	if !ok {
		return res
	}

	switch {
	case len(keys) > 1:
		return usageError("status", "name at most one plugin key")
	case len(keys) == 1 && *team != "":
		return usageError("status", "-team filters the list of plugins: name a key or a team, not both")
	}

	ctx := context.Background()

	client, res, ok := common.client(ctx, "status", env)
	if !ok {
		return res
	}

	if len(keys) == 1 {
		var plugin adminjson.Plugin

		err := client.get(ctx, client.stateURL(keys[0]), &plugin)
		if err == nil {
			err = common.show(env.Stdout, &plugin, func(out io.Writer) error { return describe(out, &plugin) })
		}

		return failure("status", err)
	}

	var list adminjson.Plugins

	err := client.get(ctx, client.listURL(), &list)
	if err == nil {
		list.Plugins = ownedBy(list.Plugins, *team)
		err = common.show(env.Stdout, &list, func(out io.Writer) error { return listPlugins(out, list.Plugins) })
	}

	return failure("status", err)
}

// runEvents runs `plg events`.
func runEvents(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	common := newAdminFlags(flags)

	keys, res, ok := parseMixed("events", flags, args)
	if !ok {
		return res
	}

	if len(keys) != 1 {
		return usageError("events", "name one plugin key")
	}

	ctx := context.Background()

	client, res, ok := common.client(ctx, "events", env)
	if !ok {
		return res
	}

	var list adminjson.Events

	err := client.get(ctx, client.eventsURL(keys[0]), &list)
	if err == nil {
		err = common.show(env.Stdout, &list, func(out io.Writer) error { return listEvents(out, list.Events) })
	}

	return failure("events", err)
}

// ownedBy returns the plugins whose owner team is team; all of them for an empty team.
func ownedBy(plugins []adminjson.Plugin, team string) []adminjson.Plugin {
	if team == "" {
		return plugins
	}

	owned := plugins[:0]

	for i := range plugins {
		if plugins[i].Owner.Team.Name == team {
			owned = append(owned, plugins[i])
		}
	}

	return owned
}

// adminFlags are the flags status and events share.
type adminFlags struct {
	profile, format *string
}

func newAdminFlags(flags *flag.FlagSet) adminFlags {
	return adminFlags{
		profile: flags.String("profile", "", "the environment, a profile of the profiles file"),
		format:  flags.String("format", formatTable, "table or json"),
	}
}

// client checks the format and returns the client of the chosen profile's admin handler.
func (f adminFlags) client(ctx context.Context, cmd string, env *Env) (*adminClient, outcome, bool) {
	if *f.format != formatTable && *f.format != formatJSON {
		return nil, usageError(cmd, fmt.Sprintf("-format must be table or json, not %q", *f.format)), false
	}

	prof, err := pickProfile(env, *f.profile)
	if err != nil {
		return nil, failure(cmd, err), false
	}

	token, err := prof.token(ctx, env, adminTimeout)
	if err != nil {
		return nil, failure(cmd, err), false
	}

	return &adminClient{
		http: &http.Client{Timeout: adminTimeout}, base: prof.base, token: token, profile: prof.Name,
	}, outcome{}, true
}

// show writes answer to out in the chosen format: indented JSON, or what table writes.
func (f adminFlags) show(out io.Writer, answer any, table func(io.Writer) error) error {
	if *f.format == formatTable {
		return table(out)
	}

	encoder := sonic.ConfigStd.NewEncoder(out)
	encoder.SetIndent("", "  ")

	return encoder.Encode(answer) //nolint:wrapcheck // writing the answer; the command prefixes its name
}

// parseMixed parses a command's flags wherever they are among its arguments, and returns the others: Go's flag
// package stops at the first argument that is not a flag, but `plg events <key> -format json` should work too.
// Everything after "--" is an argument.
func parseMixed(cmd string, flags *flag.FlagSet, args []string) ([]string, outcome, bool) {
	flags.SetOutput(io.Discard)

	var positional []string

	for {
		err := flags.Parse(args)

		switch {
		case errors.Is(err, flag.ErrHelp):
			return nil, outcome{code: exitUsage, msg: usage}, false
		case err != nil:
			return nil, usageError(cmd, err.Error()), false
		}

		rest := flags.Args()
		if len(rest) == 0 {
			return positional, outcome{}, true
		}

		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), outcome{}, true
		}

		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// adminClient reads the admin handler of a profile's environment.
type adminClient struct {
	http    *http.Client
	base    *url.URL // the profile's URL, where the service mounts the handler
	token   string   // sent as a bearer token; none when empty
	profile string
}

// listURL returns the URL of the list of plugins.
func (c *adminClient) listURL() *url.URL {
	return c.under("/plugins", "/plugins")
}

// stateURL returns the URL of the plugin key's state.
func (c *adminClient) stateURL(key string) *url.URL {
	return c.under("/plugins/"+key, "/plugins/"+escapedKey(key))
}

// eventsURL returns the URL of the plugin key's events.
func (c *adminClient) eventsURL(key string) *url.URL {
	return c.under("/plugins/"+key+"/events", "/plugins/"+escapedKey(key)+"/events")
}

// escapedKey returns key escaped for a URL path. Its slashes stay literal, but the one before a last part
// "events", which the handler would take for the events path: that one is escaped.
func escapedKey(key string) string {
	parts := strings.Split(key, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}

	if n := len(parts); n > 1 && parts[n-1] == "events" {
		return strings.Join(parts[:n-1], "/") + "%2Fevents"
	}

	return strings.Join(parts, "/")
}

// under returns the URL of path under the base URL, given as is and escaped.
func (c *adminClient) under(path, escaped string) *url.URL {
	target := *c.base
	target.Path = strings.TrimSuffix(c.base.Path, "/") + path
	target.RawPath = strings.TrimSuffix(c.base.EscapedPath(), "/") + escaped

	return &target
}

// get GETs target and decodes its JSON answer into out. Any answer but 200 is an error, with the handler's
// message when it gave one.
func (c *adminClient) get(ctx context.Context, target *url.URL, out any) error {
	req := (&http.Request{Method: http.MethodGet, URL: target, Host: target.Host, Header: http.Header{}}).
		WithContext(ctx)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "plg")

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	answer, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("profile %s: %w", c.profile, err)
	}

	defer answer.Body.Close() //nolint:errcheck // read only: closing cannot lose data

	if answer.StatusCode != http.StatusOK {
		return c.refused(target, answer)
	}

	err = sonic.ConfigDefault.NewDecoder(answer.Body).Decode(out)
	if err != nil {
		return fmt.Errorf("profile %s: GET %s: reading the answer: %w", c.profile, target.Redacted(), err)
	}

	return nil
}

// refused explains an answer other than 200: its status and the handler's message ({"error": …}) or, from
// anything else (the service's own authentication, a proxy), the start of its body, on one line without control
// characters.
func (c *adminClient) refused(target *url.URL, answer *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(answer.Body, maxErrorAnswer))
	if err != nil { // the status alone says what happened
		body = nil
	}

	msg, text := answer.Status, string(body)

	var decoded adminjson.Error

	if sonic.ConfigDefault.Unmarshal(body, &decoded) == nil && decoded.Error != "" {
		text = decoded.Error
	}

	if text = oneLine(text); text != "" {
		msg += ": " + text
	}

	if answer.StatusCode == http.StatusUnauthorized || answer.StatusCode == http.StatusForbidden {
		msg += " (check the profile's token)"
	}

	return fmt.Errorf("profile %s: GET %s: %w %s", c.profile, target.Redacted(), errAnswer, msg)
}
