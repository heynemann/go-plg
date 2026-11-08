package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/heynemann/go-plg/internal/strictyaml"
)

// profilesName is the profiles file, under the user's configuration directory.
const profilesName = "plg/profiles.yaml"

// maxProfilesSize is the largest profiles file plg reads.
const maxProfilesSize = 1 << 20

var (
	errNoConfigDir     = errors.New("neither XDG_CONFIG_HOME (absolute) nor HOME is set")
	errProfilesSize    = fmt.Errorf("is over %d bytes", maxProfilesSize)
	errProfileName     = errors.New("a profile needs a name")
	errProfileTwice    = errors.New("is named by two profiles")
	errProfileURL      = errors.New("url must be an absolute http or https URL")
	errTokenSource     = errors.New("token needs exactly one of env and command")
	errNoDefault       = errors.New("default names no profile")
	errNoProfiles      = errors.New("holds no profiles")
	errUnknownProfile  = errors.New("no such profile")
	errPickProfile     = errors.New("name a profile with -profile")
	errTokenUnset      = errors.New("is not set")
	errTokenCommand    = errors.New("token command failed")
	errTokenEmpty      = errors.New("token command printed no token")
	errProfilesMissing = errors.New("does not exist: list the environments there (see plg's usage)")
)

// profiles is the content of the profiles file: the environments `plg status` and `plg events` read.
type profiles struct {
	Default  string    `yaml:"default"`
	Profiles []profile `yaml:"profiles"`
}

// profile is an environment: the URL its service mounts plg.AdminHandler at, and where the token is.
type profile struct {
	Token *tokenSource `yaml:"token"`
	base  *url.URL     // URL, parsed by check
	Name  string       `yaml:"name"`
	URL   string       `yaml:"url"`
}

// tokenSource says where a profile's bearer token comes from: an environment variable, or what a command (run
// without a shell, in the working directory) prints. A profile without one sends no token.
type tokenSource struct {
	Env     string   `yaml:"env"`
	Command []string `yaml:"command"`
}

// profilesPath returns where the profiles file is: under $XDG_CONFIG_HOME when it is absolute (the XDG rule),
// else under ~/.config.
func profilesPath(env *Env) (string, error) {
	if dir := env.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, profilesName), nil
	}

	if home := env.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", profilesName), nil
	}

	return "", fmt.Errorf("finding %s: %w", profilesName, errNoConfigDir)
}

// pickProfile reads the profiles file and returns the profile named name, or the default one for an empty name.
func pickProfile(env *Env, name string) (*profile, error) {
	path, err := profilesPath(env)
	if err != nil {
		return nil, err
	}

	all, err := readProfiles(path)
	if err != nil {
		return nil, err
	}

	return all.pick(path, name)
}

// readProfiles reads and checks the profiles file at path.
func readProfiles(path string) (*profiles, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the user's own configuration file
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s %w", path, errProfilesMissing)
	}

	if err != nil {
		return nil, fmt.Errorf("reading profiles: %w", err)
	}

	defer file.Close() //nolint:errcheck // read only: closing cannot lose data

	data, err := io.ReadAll(io.LimitReader(file, maxProfilesSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading profiles: %w", err)
	}

	if len(data) > maxProfilesSize {
		return nil, fmt.Errorf("%s %w", path, errProfilesSize)
	}

	var all profiles

	err = strictyaml.Decode(data, filepath.Base(path), &all)
	if err == nil {
		err = all.check()
	}

	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return &all, nil
}

// check checks every profile and the default.
func (p *profiles) check() error {
	for i := range p.Profiles {
		prof := &p.Profiles[i]

		err := prof.check()
		if err != nil {
			return err
		}

		if slices.ContainsFunc(p.Profiles[:i], func(other profile) bool { return other.Name == prof.Name }) {
			return fmt.Errorf("%q %w", prof.Name, errProfileTwice)
		}
	}

	if p.Default != "" && !slices.ContainsFunc(p.Profiles, func(prof profile) bool { return prof.Name == p.Default }) {
		return fmt.Errorf("%w %q", errNoDefault, p.Default)
	}

	return nil
}

// check checks the profile's name, URL and token source.
func (p *profile) check() error {
	if p.Name == "" {
		return errProfileName
	}

	base, err := url.Parse(p.URL)
	if err != nil { // the URL is not quoted: it may hold credentials, which a parse failure cannot redact
		return fmt.Errorf("profile %s: %w, not a URL", p.Name, errProfileURL)
	}

	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("profile %s: %w, not %q", p.Name, errProfileURL, base.Redacted())
	}

	p.base = base

	if p.Token != nil && (p.Token.Env == "") == (len(p.Token.Command) == 0) {
		return fmt.Errorf("profile %s: %w", p.Name, errTokenSource)
	}

	return nil
}

// pick returns the profile named name; for an empty name, the default, or the only profile.
func (p *profiles) pick(path, name string) (*profile, error) {
	names := make([]string, len(p.Profiles))
	for i := range p.Profiles {
		names[i] = p.Profiles[i].Name
	}

	if name == "" {
		name = p.Default
	}

	switch {
	case len(p.Profiles) == 0:
		return nil, fmt.Errorf("%s %w", path, errNoProfiles)
	case name == "" && len(p.Profiles) == 1:
		return &p.Profiles[0], nil
	case name == "":
		return nil, fmt.Errorf("%w (%s has %s and no default)", errPickProfile, path, strings.Join(names, ", "))
	}

	i := slices.Index(names, name)
	if i < 0 {
		return nil, fmt.Errorf("%w %q in %s (it has %s)", errUnknownProfile, name, path, strings.Join(names, ", "))
	}

	return &p.Profiles[i], nil
}

// token returns the profile's bearer token; empty when the profile names no token source. A token command has
// timeout to print it.
func (p *profile) token(ctx context.Context, env *Env, timeout time.Duration) (string, error) {
	switch {
	case p.Token == nil:
		return "", nil
	case p.Token.Env != "":
		token := env.Getenv(p.Token.Env)
		if token == "" {
			return "", fmt.Errorf("profile %s: the token's variable %s %w", p.Name, p.Token.Env, errTokenUnset)
		}

		return token, nil
	}

	var stdout, stderr bytes.Buffer

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	//nolint:gosec // G204: runs the command the user's own profile names
	cmd := exec.CommandContext(ctx, p.Token.Command[0], p.Token.Command[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = env.Dir, &stdout, &stderr
	cmd.WaitDelay = time.Second // its children may hold its output open once it is killed

	err := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("profile %s: %w: %q did not finish within %s", p.Name, errTokenCommand,
			p.Token.Command, timeout)
	}

	if err != nil {
		return "", fmt.Errorf("profile %s: %w: %q: %w: %s", p.Name, errTokenCommand, p.Token.Command, err,
			strings.TrimSpace(stderr.String()))
	}

	token := strings.TrimSpace(stdout.String())
	if token == "" {
		return "", fmt.Errorf("profile %s: %w: %q", p.Name, errTokenEmpty, p.Token.Command)
	}

	return token, nil
}
