// Package adminjson is the JSON form of plg's admin handler: what plg.AdminHandler encodes and the plg command
// (`plg status`, `plg events`) decodes, so both sides share one definition. What each key means is documented on
// plg.AdminHandler; the types here mirror plg's (PluginState, Report, Issue, PanicError, PoolStats, Owner, Event),
// with errors as their messages, times in UTC, durations in nanoseconds and memory in bytes.
package adminjson

import "time"

// Plugins is the answer to GET /plugins.
type Plugins struct {
	Plugins []Plugin `json:"plugins"`
}

// Events is the answer to GET /plugins/{key}/events.
type Events struct {
	Events []Event `json:"events"`
}

// Error is an error answer.
type Error struct {
	Error string `json:"error"`
}

// Plugin is a plg.PluginState.
type Plugin struct {
	LoadedAt        *time.Time `json:"loadedAt"`
	Load            *Report    `json:"load"`
	Validation      *Report    `json:"validation"`
	Rejection       *Report    `json:"rejection"`
	LastPanic       *Panic     `json:"lastPanic"`
	Pool            *Pool      `json:"pool"`
	Key             string     `json:"key"`
	Registration    string     `json:"registration"`
	Storage         string     `json:"storage"`
	Language        string     `json:"language"`
	Status          string     `json:"status"`
	Version         string     `json:"version"`
	RejectedVersion string     `json:"rejectedVersion"`
	Owner           Owner      `json:"owner"`
	Panics          uint64     `json:"panics"`
}

// Report is a plg.Report.
type Report struct {
	At       time.Time `json:"at"`
	Error    *string   `json:"error"`
	Owner    Owner     `json:"owner"`
	Key      string    `json:"key"`
	Version  string    `json:"version"`
	Issues   []Issue   `json:"issues"`
	Duration int64     `json:"durationNs"`
}

// Issue is a plg.Issue.
type Issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Field    string `json:"field"`
}

// Panic is a plg.PanicError.
type Panic struct {
	Time    time.Time `json:"time"`
	Phase   string    `json:"phase"`
	Method  string    `json:"method"`
	Message string    `json:"message"` // Error()
	Value   string    `json:"value"`   // the recovered value, the guest's panic message or the trap
	Guest   string    `json:"guest"`
	Stack   string    `json:"stack"`
}

// Pool is a plg.PoolStats.
type Pool struct {
	Min           int    `json:"min"`
	Max           int    `json:"max"`
	Instances     int    `json:"instances"`
	InUse         int    `json:"inUse"`
	CallsInFlight int    `json:"callsInFlight"`
	IdleSlots     int    `json:"idleSlots"`
	Waiting       int    `json:"waiting"`
	Refills       uint64 `json:"refills"`
	Grows         uint64 `json:"grows"`
	MemoryLimit   uint64 `json:"memoryLimitBytes"`
	Memory        uint64 `json:"memoryBytes"`
}

// Owner is a plg.Owner.
type Owner struct {
	Labels      map[string]string `json:"labels"`
	Team        Team              `json:"team"`
	Org         string            `json:"org"`
	Company     string            `json:"company"`
	Contact     Contact           `json:"contact"`
	Links       Links             `json:"links"`
	Maintainers []Maintainer      `json:"maintainers"`
}

// Team is a plg.Team.
type Team struct {
	Name       string `json:"name"`
	Department string `json:"department"`
}

// Contact is a plg.Contact.
type Contact struct {
	Oncall  string `json:"oncall"`
	Channel string `json:"channel"`
	Email   string `json:"email"`
}

// Links is a plg.Links.
type Links struct {
	Repository string `json:"repository"`
	Docs       string `json:"docs"`
	Runbook    string `json:"runbook"`
	Issues     string `json:"issues"`
}

// Maintainer is a plg.Maintainer.
type Maintainer struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	Handle string `json:"handle"`
}

// Event is a plg.Event.
type Event struct {
	At       time.Time `json:"at"`
	Report   *Report   `json:"report"`
	Panic    *Panic    `json:"panic"`
	Kind     string    `json:"kind"`
	Key      string    `json:"key"`
	Version  string    `json:"version"`
	Language string    `json:"language"`
	Storage  string    `json:"storage"`
	Owner    Owner     `json:"owner"`
}
