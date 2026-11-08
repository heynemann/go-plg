package plg

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

// The limits of an [Owner], checked by [ParseManifest] and by Register for every descriptor.
const (
	// MaxOwnerStringBytes is the longest owner string (a name, an address, a link, a label's key or value), in
	// bytes.
	MaxOwnerStringBytes = 256
	// MaxOwnerMaintainers is the most maintainers an owner may list.
	MaxOwnerMaintainers = 16
	// MaxOwnerLabels is the most labels an owner may have.
	MaxOwnerLabels = 32
)

// Owner says who owns a plugin, so that whoever meets a problem with it knows who to talk to. It comes from the
// owner block of the plugin's plugin.yaml (see [FSStorage]) through [Descriptor.Owner], and plg shows it on the
// plugin's [PanicError] and [PluginError] values, on every log record it writes about the plugin (as an "owner"
// group of the fields that are set) and on every plugin metric, whose team, org and company attributes are
// Team.Name, Org and Company (empty when not set; names, addresses and labels never become metric attributes).
// The log group holds team, department, org, company, the primary maintainer (maintainer, maintainer_email,
// maintainer_handle), oncall, channel, email, repository, docs, runbook, issues and a labels group.
//
// Every field is optional. Strings are at most [MaxOwnerStringBytes] bytes of UTF-8; there are at most
// [MaxOwnerMaintainers] maintainers, each with at least one field set, and at most [MaxOwnerLabels] labels, whose
// keys are not empty. An Owner plg hands out shares its slice and map with the plugin's descriptor: do not modify
// them.
type Owner struct {
	// Labels are free-form key-value pairs, logged but never metric attributes.
	Labels map[string]string `yaml:"labels"`
	// Team is the team owning the plugin.
	Team Team `yaml:"team"`
	// Org is the organization the team belongs to.
	Org string `yaml:"org"`
	// Company is the company the organization belongs to.
	Company string `yaml:"company"`
	// Contact is how to reach the owners.
	Contact Contact `yaml:"contact"`
	// Links point to the plugin's resources.
	Links Links `yaml:"links"`
	// Maintainers are the people maintaining the plugin; the first is the primary one.
	Maintainers []Maintainer `yaml:"maintainers"`
}

// Maintainer is a person maintaining a plugin.
type Maintainer struct {
	Name   string `yaml:"name"`
	Email  string `yaml:"email"`
	Handle string `yaml:"handle"` // a chat or source-hosting handle
}

// Team is the team owning a plugin.
type Team struct {
	Name       string `yaml:"name"`
	Department string `yaml:"department"`
}

// Contact is how to reach a plugin's owners.
type Contact struct {
	Oncall  string `yaml:"oncall"`  // an on-call rotation or pager
	Channel string `yaml:"channel"` // a chat channel
	Email   string `yaml:"email"`
}

// Links point to a plugin's resources.
type Links struct {
	Repository string `yaml:"repository"`
	Docs       string `yaml:"docs"`
	Runbook    string `yaml:"runbook"`
	Issues     string `yaml:"issues"`
}

var (
	errOwnerMaintainers = errors.New("owner has too many maintainers")
	errOwnerLabels      = errors.New("owner has too many labels")
	errOwnerString      = fmt.Errorf("owner strings must be valid UTF-8 of at most %d bytes", MaxOwnerStringBytes)
	errOwnerMaintainer  = errors.New("an owner maintainer needs a name, an email or a handle")
	errOwnerLabelKey    = errors.New("owner label keys must not be empty")
)

// check reports the first limit o breaks.
func (o *Owner) check() error {
	if len(o.Maintainers) > MaxOwnerMaintainers {
		return fmt.Errorf("%w: %d, limit %d", errOwnerMaintainers, len(o.Maintainers), MaxOwnerMaintainers)
	}

	if len(o.Labels) > MaxOwnerLabels {
		return fmt.Errorf("%w: %d, limit %d", errOwnerLabels, len(o.Labels), MaxOwnerLabels)
	}

	for i, m := range o.Maintainers {
		if m == (Maintainer{}) {
			return fmt.Errorf("maintainer %d: %w", i, errOwnerMaintainer)
		}
	}

	if _, ok := o.Labels[""]; ok {
		return errOwnerLabelKey
	}

	var bad string

	o.eachString(func(field, value string) bool {
		if len(value) > MaxOwnerStringBytes || !utf8.ValidString(value) {
			bad = field
		}

		return bad == ""
	})

	if bad != "" {
		return fmt.Errorf("%s: %w", bad, errOwnerString)
	}

	return nil
}

// eachString calls fn with the name and value of every string of o, label keys included, until fn returns false.
func (o *Owner) eachString(fn func(field, value string) bool) {
	fields := []struct{ name, value string }{
		{"team.name", o.Team.Name},
		{"team.department", o.Team.Department},
		{"org", o.Org},
		{"company", o.Company},
		{"contact.oncall", o.Contact.Oncall},
		{"contact.channel", o.Contact.Channel},
		{"contact.email", o.Contact.Email},
		{"links.repository", o.Links.Repository},
		{"links.docs", o.Links.Docs},
		{"links.runbook", o.Links.Runbook},
		{"links.issues", o.Links.Issues},
	}

	for _, f := range fields {
		if !fn(f.name, f.value) {
			return
		}
	}

	for i, m := range o.Maintainers {
		for _, f := range []struct{ name, value string }{{"name", m.Name}, {"email", m.Email}, {"handle", m.Handle}} {
			if !fn(fmt.Sprintf("maintainers[%d].%s", i, f.name), f.value) {
				return
			}
		}
	}

	for _, key := range slices.Sorted(maps.Keys(o.Labels)) {
		if !fn("labels key", key) || !fn("labels."+key, o.Labels[key]) {
			return
		}
	}
}

// equal reports whether o and p are the same owner (a nil and an empty labels map are the same).
func (o *Owner) equal(p *Owner) bool {
	return o.Team == p.Team && o.Org == p.Org && o.Company == p.Company && o.Contact == p.Contact &&
		o.Links == p.Links && slices.Equal(o.Maintainers, p.Maintainers) && maps.Equal(o.Labels, p.Labels)
}

// logAttr returns the "owner" log attribute of o: a group of the fields that are set, which slog's handlers leave
// out when there are none.
func (o *Owner) logAttr() slog.Attr {
	var attrs []any

	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}

	add("team", o.Team.Name)
	add("department", o.Team.Department)
	add("org", o.Org)
	add("company", o.Company)

	if len(o.Maintainers) > 0 {
		primary := o.Maintainers[0]
		add("maintainer", primary.Name)
		add("maintainer_email", primary.Email)
		add("maintainer_handle", primary.Handle)
	}

	add("oncall", o.Contact.Oncall)
	add("channel", o.Contact.Channel)
	add("email", o.Contact.Email)
	add("repository", o.Links.Repository)
	add("docs", o.Links.Docs)
	add("runbook", o.Links.Runbook)
	add("issues", o.Links.Issues)

	if len(o.Labels) > 0 {
		labels := make([]any, 0, len(o.Labels))
		for _, key := range slices.Sorted(maps.Keys(o.Labels)) {
			labels = append(labels, slog.String(key, o.Labels[key]))
		}

		attrs = append(attrs, slog.Group("labels", labels...))
	}

	return slog.Group("owner", attrs...)
}

// metricAttrs returns the team, org and company metric attributes of o, empty strings when not set.
func (o *Owner) metricAttrs() [3]attribute.KeyValue {
	return [3]attribute.KeyValue{
		attribute.String("team", o.Team.Name), attribute.String("org", o.Org), attribute.String("company", o.Company),
	}
}

// ownerTags are a plugin version's owner and the forms plg reports it in, built once when the version loads so
// that reporting it allocates nothing more, with the version and the storage it came from, which the version's
// [PanicError]s carry to their [EventPanicked] events.
type ownerTags struct {
	metrics [3]attribute.KeyValue // team, org and company
	log     slog.Attr             // the "owner" log group
	version string                // the plugin version, resolved
	storage string                // the name of the storage it was loaded from
	owner   Owner                 // what PanicError and PluginError carry
}

// newOwnerTags returns the tags of owner, for a plugin version loaded from storage.
func newOwnerTags(owner *Owner, version, storage string) *ownerTags {
	return &ownerTags{
		owner: *owner, log: owner.logAttr(), metrics: owner.metricAttrs(), version: version, storage: storage,
	}
}

// noOwner are the tags of a plugin without an owner, and of what is no plugin.
var noOwner = newOwnerTags(&Owner{}, "", "") //nolint:gochecknoglobals // immutable, shared by owner-less reports

// pluginID identifies a plugin version on its reports, log records, measurements and spans: its key, its language
// and its owner's tags. prepareLaunch builds it once per version, and what serves the version's instances (its
// meters, spans, launcher and HTTP and Host handlers) reads it; it is passed by value, so it allocates nothing.
type pluginID struct {
	owner *ownerTags
	key   string
	lang  Language
}
