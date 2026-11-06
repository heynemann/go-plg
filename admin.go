package plg

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// AdminHandler serves what [Plugins] and [Info] describe as read-only JSON, for dashboards and admin pages:
//
//   - GET /plugins answers {"plugins": [<plugin>, …]}, every plugin in key order;
//   - GET /plugins/{key} answers the <plugin> with that key, "<name>/<descriptor key>" (its slash may stay as
//     is; escape anything else a URL path cannot hold), or 404 when [Info] does not know it.
//
// A <plugin> is an object with the keys "key", "registration", "storage", "language", "status" (a [Status]),
// "version", "rejectedVersion", "loadedAt", "owner", "pool", "panics", "lastPanic", "load", "validation" and
// "rejection", as in [PluginState]. A <report> ("load", "validation", "rejection") has "key", "version", "at",
// "durationNs", "error", "owner" and "issues" (each with "severity", "code", "message" and "field"); a
// "lastPanic" has "time", "phase", "method", "message", "value", "guest" and "stack"; "pool" has "min", "max",
// "instances", "inUse", "callsInFlight", "idleSlots", "waiting", "refills", "grows", "memoryLimitBytes" and
// "memoryBytes"; "owner" has "team" ("name", "department"), "org", "company", "contact" ("oncall", "channel",
// "email"), "links" ("repository", "docs", "runbook", "issues"), "maintainers" (each with "name", "email",
// "handle") and "labels". Every key is always present: a report, "lastPanic", "pool" and "loadedAt" are null when
// the state has none (the pool when no version serves), "error" is null when the report has none, and lists and
// labels are empty rather than null. Errors and panic values are their messages. Times are RFC 3339 strings with
// nanoseconds, in UTC; durations are integer nanoseconds; memory is in bytes.
//
// Any other path answers 404, and any other method on these paths 405 with "Allow: GET" (HEAD included). Errors
// answer {"error": "<message>"}; a closed container answers 503. Every answer is JSON with Cache-Control:
// no-store. The handler reads what the container keeps, never waiting for a load or a call, and runs no plugin
// code: validation, reloads and [Check] stay Go calls.
//
// The answers hold the plugins' owners, maintainers' names and emails and on-call contacts included, and their
// panics' stacks: mount the handler behind the service's own authentication and authorization, never on a
// public listener. It serves the paths above as given, so mount it under a prefix with [http.StripPrefix]:
//
//	mux.Handle("/admin/plg/", requireAdmin(http.StripPrefix("/admin/plg", plg.AdminHandler(c))))
//
// It is safe for concurrent use.
func AdminHandler(ctr *Container) http.Handler {
	return &adminHandler{ctr: ctr}
}

// adminHandler is the [AdminHandler] of ctr.
type adminHandler struct {
	ctr *Container
}

func (h *adminHandler) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	key, one := strings.CutPrefix(path, "/plugins/")

	switch {
	case path != "/plugins" && (!one || key == ""):
		h.answer(writer, req, http.StatusNotFound, adminError{Error: "plg: no admin resource at " + path})
	case req.Method != http.MethodGet:
		writer.Header().Set("Allow", http.MethodGet)
		h.answer(writer, req, http.StatusMethodNotAllowed,
			adminError{Error: "plg: the admin handler is read-only: " + req.Method + " is not allowed, only GET"})
	case one:
		h.plugin(writer, req, key)
	default:
		h.plugins(writer, req)
	}
}

// plugins answers GET /plugins.
func (h *adminHandler) plugins(writer http.ResponseWriter, req *http.Request) {
	states, open := h.ctr.plugins()
	if !open {
		h.answer(writer, req, http.StatusServiceUnavailable, adminError{Error: ErrClosed.Error()})

		return
	}

	list := adminPlugins{Plugins: make([]adminState, len(states))}
	for i := range states {
		list.Plugins[i] = newAdminState(&states[i])
	}

	h.answer(writer, req, http.StatusOK, list)
}

// plugin answers GET /plugins/{key}.
func (h *adminHandler) plugin(writer http.ResponseWriter, req *http.Request, key string) {
	state, err := Info(h.ctr, key)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, ErrClosed) {
			status = http.StatusServiceUnavailable
		}

		h.answer(writer, req, status, adminError{Error: err.Error()})

		return
	}

	h.answer(writer, req, http.StatusOK, newAdminState(&state))
}

// answer writes body as the JSON answer with the given status.
func (h *adminHandler) answer(writer http.ResponseWriter, req *http.Request, status int, body any) {
	header := writer.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)

	// ConfigStd sorts the labels and replaces invalid UTF-8 (a plugin's issue message may hold any bytes).
	err := sonic.ConfigStd.NewEncoder(writer).Encode(body)
	if err != nil { // the client is gone: the answer has started, nothing else can be sent
		h.ctr.logger.LogAttrs(req.Context(), slog.LevelDebug, "plg: admin answer not written",
			slog.String("path", req.URL.Path), slog.String("error", err.Error()))
	}
}

// adminPlugins is the answer to GET /plugins.
type adminPlugins struct {
	Plugins []adminState `json:"plugins"`
}

// adminError is an error answer.
type adminError struct {
	Error string `json:"error"`
}

// adminState is a [PluginState] as the admin handler answers it.
type adminState struct {
	LoadedAt        *time.Time   `json:"loadedAt"`
	Load            *adminReport `json:"load"`
	Validation      *adminReport `json:"validation"`
	Rejection       *adminReport `json:"rejection"`
	LastPanic       *adminPanic  `json:"lastPanic"`
	Pool            *adminPool   `json:"pool"`
	Key             string       `json:"key"`
	Registration    string       `json:"registration"`
	Storage         string       `json:"storage"`
	Language        Language     `json:"language"`
	Status          Status       `json:"status"`
	Version         string       `json:"version"`
	RejectedVersion string       `json:"rejectedVersion"`
	Owner           adminOwner   `json:"owner"`
	Panics          uint64       `json:"panics"`
}

func newAdminState(src *PluginState) adminState {
	state := adminState{
		Load:            newAdminReport(src.Load),
		Validation:      newAdminReport(src.Validation),
		Rejection:       newAdminReport(src.Rejection),
		LastPanic:       newAdminPanic(src.LastPanic),
		Key:             src.Key,
		Registration:    src.Registration,
		Storage:         src.Storage,
		Language:        src.Language,
		Status:          src.Status,
		Version:         src.Version,
		RejectedVersion: src.RejectedVersion,
		Owner:           newAdminOwner(&src.Owner),
		Panics:          src.Panics,
	}

	if src.Load != nil { // a version serves
		loaded := src.LoadedAt.UTC()
		pool := adminPool(src.Pool)
		state.LoadedAt, state.Pool = &loaded, &pool
	}

	return state
}

// adminReport is a [Report] as the admin handler answers it.
type adminReport struct {
	At       time.Time    `json:"at"`
	Error    *string      `json:"error"`
	Owner    adminOwner   `json:"owner"`
	Key      string       `json:"key"`
	Version  string       `json:"version"`
	Issues   []adminIssue `json:"issues"`
	Duration int64        `json:"durationNs"`
}

func newAdminReport(src *Report) *adminReport {
	if src == nil {
		return nil
	}

	report := &adminReport{
		At:       src.At.UTC(),
		Owner:    newAdminOwner(&src.Owner),
		Key:      src.Key,
		Version:  src.Version,
		Issues:   make([]adminIssue, len(src.Issues)),
		Duration: src.Duration.Nanoseconds(),
	}

	for i, issue := range src.Issues {
		report.Issues[i] = adminIssue(issue)
	}

	if src.Err != nil {
		msg := src.Err.Error()
		report.Error = &msg
	}

	return report
}

// adminIssue is an [Issue] as the admin handler answers it.
type adminIssue struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Field    string   `json:"field"`
}

// adminPanic is a [PanicError] as the admin handler answers it.
type adminPanic struct {
	Time    time.Time `json:"time"`
	Phase   Phase     `json:"phase"`
	Method  string    `json:"method"`
	Message string    `json:"message"` // Error()
	Value   string    `json:"value"`   // the recovered value, the guest's panic message or the trap
	Guest   string    `json:"guest"`
	Stack   string    `json:"stack"`
}

func newAdminPanic(src *PanicError) *adminPanic {
	if src == nil {
		return nil
	}

	return &adminPanic{
		Time:    src.Time.UTC(),
		Phase:   src.Phase,
		Method:  src.Method,
		Message: src.Error(),
		Value:   fmt.Sprint(src.Value),
		Guest:   src.Guest,
		Stack:   string(src.Stack),
	}
}

// adminPool is a [PoolStats] as the admin handler answers it.
type adminPool struct {
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

// adminOwner is an [Owner] as the admin handler answers it.
type adminOwner struct {
	Labels      map[string]string `json:"labels"`
	Team        adminTeam         `json:"team"`
	Org         string            `json:"org"`
	Company     string            `json:"company"`
	Contact     adminContact      `json:"contact"`
	Links       adminLinks        `json:"links"`
	Maintainers []adminMaintainer `json:"maintainers"`
}

func newAdminOwner(src *Owner) adminOwner {
	owner := adminOwner{
		Labels:      src.Labels,
		Team:        adminTeam(src.Team),
		Org:         src.Org,
		Company:     src.Company,
		Contact:     adminContact(src.Contact),
		Links:       adminLinks(src.Links),
		Maintainers: make([]adminMaintainer, len(src.Maintainers)),
	}

	if owner.Labels == nil {
		owner.Labels = map[string]string{}
	}

	for i, maintainer := range src.Maintainers {
		owner.Maintainers[i] = adminMaintainer(maintainer)
	}

	return owner
}

// adminTeam, adminContact, adminLinks and adminMaintainer are [Team], [Contact], [Links] and [Maintainer] as the
// admin handler answers them.
type (
	adminTeam struct {
		Name       string `json:"name"`
		Department string `json:"department"`
	}
	adminContact struct {
		Oncall  string `json:"oncall"`
		Channel string `json:"channel"`
		Email   string `json:"email"`
	}
	adminLinks struct {
		Repository string `json:"repository"`
		Docs       string `json:"docs"`
		Runbook    string `json:"runbook"`
		Issues     string `json:"issues"`
	}
	adminMaintainer struct {
		Name   string `json:"name"`
		Email  string `json:"email"`
		Handle string `json:"handle"`
	}
)
