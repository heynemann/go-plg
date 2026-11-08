package plg

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg/internal/adminjson"
)

// AdminHandler serves what [Plugins], [Info] and [Events] describe as read-only JSON, for dashboards, admin pages
// and the plg command (`plg status`, `plg events`):
//
//   - GET /plugins answers {"plugins": [<plugin>, …]}, every plugin in key order;
//   - GET /plugins/{key} answers the <plugin> with that key, "<name>/<descriptor key>" (its slash may stay as
//     is; escape anything else a URL path cannot hold), or 404 when [Info] does not know it;
//   - GET /plugins/{key}/events answers {"events": [<event>, …]}, the plugin's newest events (at most 100) oldest
//     first, or 404 when [Info] does not know the key. A literal "/events" ends the path: a key whose own last
//     part is "events" (a storage whose keys hold slashes) reads its <plugin> with that slash escaped as %2F.
//
// A <plugin> is an object with the keys "key", "registration", "storage", "language", "status" (a [Status]),
// "version", "rejectedVersion", "loadedAt", "owner", "pool", "panics", "lastPanic", "load", "validation" and
// "rejection", as in [PluginState]. A <report> ("load", "validation", "rejection") has "key", "version", "at",
// "durationNs", "error", "owner" and "issues" (each with "severity", "code", "message" and "field"); a
// "lastPanic" has "time", "phase", "method", "message", "value", "guest" and "stack"; "pool" has "min", "max",
// "instances", "inUse", "callsInFlight", "idleSlots", "waiting", "refills", "grows", "memoryLimitBytes" and
// "memoryBytes"; "owner" has "team" ("name", "department"), "org", "company", "contact" ("oncall", "channel",
// "email"), "links" ("repository", "docs", "runbook", "issues"), "maintainers" (each with "name", "email",
// "handle") and "labels". An <event> has "at", "kind" (an [EventKind]), "key", "version", "language", "storage",
// "owner", "report" (a <report>) and "panic" (as "lastPanic"), as in [Event]. Every key is always present: a
// report, a panic, "pool" and "loadedAt" are null when the state or event has none (the pool when no version
// serves), "error" is null when the report has none, and lists and labels are empty rather than null. Errors and
// panic values are their messages. Times are RFC 3339 strings with nanoseconds, in UTC; durations are integer
// nanoseconds; memory is in bytes.
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

// eventsSuffix ends the path of a plugin's events.
const eventsSuffix = "/events"

func (h *adminHandler) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	key, one := strings.CutPrefix(path, "/plugins/")

	// Only a literal "/events" ends the events path: in an escaped one, "%2Fevents", the slash is the key's.
	events := strings.HasSuffix(key, eventsSuffix) && strings.HasSuffix(req.URL.EscapedPath(), eventsSuffix)
	if events {
		key = strings.TrimSuffix(key, eventsSuffix)
	}

	switch {
	case path != "/plugins" && (!one || key == ""):
		h.answer(writer, req, http.StatusNotFound, adminjson.Error{Error: "plg: no admin resource at " + path})
	case req.Method != http.MethodGet:
		writer.Header().Set("Allow", http.MethodGet)
		h.answer(writer, req, http.StatusMethodNotAllowed,
			adminjson.Error{Error: "plg: the admin handler is read-only: " + req.Method + " is not allowed, only GET"})
	case events:
		h.events(writer, req, key)
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
		h.failed(writer, req, ErrClosed)

		return
	}

	list := adminjson.Plugins{Plugins: make([]adminjson.Plugin, len(states))}
	for i := range states {
		list.Plugins[i] = newAdminState(&states[i])
	}

	h.answer(writer, req, http.StatusOK, list)
}

// plugin answers GET /plugins/{key}.
func (h *adminHandler) plugin(writer http.ResponseWriter, req *http.Request, key string) {
	state, err := Info(h.ctr, key)
	if err != nil {
		h.failed(writer, req, err)

		return
	}

	h.answer(writer, req, http.StatusOK, newAdminState(&state))
}

// events answers GET /plugins/{key}/events.
func (h *adminHandler) events(writer http.ResponseWriter, req *http.Request, key string) {
	events, err := Events(h.ctr, key)
	if err != nil {
		h.failed(writer, req, err)

		return
	}

	list := adminjson.Events{Events: make([]adminjson.Event, len(events))}
	for i := range events {
		list.Events[i] = newAdminEvent(&events[i])
	}

	h.answer(writer, req, http.StatusOK, list)
}

// failed answers err, from [Info] or [Events]: 503 once the container is closed, 404 otherwise (a key they do not
// know).
func (h *adminHandler) failed(writer http.ResponseWriter, req *http.Request, err error) {
	status := http.StatusNotFound
	if errors.Is(err, ErrClosed) {
		status = http.StatusServiceUnavailable
	}

	h.answer(writer, req, status, adminjson.Error{Error: err.Error()})
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

func newAdminState(src *PluginState) adminjson.Plugin {
	state := adminjson.Plugin{
		Load:            newAdminReport(src.Load),
		Validation:      newAdminReport(src.Validation),
		Rejection:       newAdminReport(src.Rejection),
		LastPanic:       newAdminPanic(src.LastPanic),
		Key:             src.Key,
		Registration:    src.Registration,
		Storage:         src.Storage,
		Language:        string(src.Language),
		Status:          string(src.Status),
		Version:         src.Version,
		RejectedVersion: src.RejectedVersion,
		Owner:           newAdminOwner(&src.Owner),
		Panics:          src.Panics,
	}

	if src.Load != nil { // a version serves
		loaded := src.LoadedAt.UTC()
		pool := adminjson.Pool(src.Pool)
		state.LoadedAt, state.Pool = &loaded, &pool
	}

	return state
}

func newAdminReport(src *Report) *adminjson.Report {
	if src == nil {
		return nil
	}

	report := &adminjson.Report{
		At:       src.At.UTC(),
		Owner:    newAdminOwner(&src.Owner),
		Key:      src.Key,
		Version:  src.Version,
		Issues:   make([]adminjson.Issue, len(src.Issues)),
		Duration: src.Duration.Nanoseconds(),
	}

	for i, issue := range src.Issues {
		report.Issues[i] = adminjson.Issue{
			Severity: string(issue.Severity), Code: issue.Code, Message: issue.Message, Field: issue.Field,
		}
	}

	if src.Err != nil {
		msg := src.Err.Error()
		report.Error = &msg
	}

	return report
}

func newAdminPanic(src *PanicError) *adminjson.Panic {
	if src == nil {
		return nil
	}

	return &adminjson.Panic{
		Time:    src.Time.UTC(),
		Phase:   string(src.Phase),
		Method:  src.Method,
		Message: src.Error(),
		Value:   fmt.Sprint(src.Value),
		Guest:   src.Guest,
		Stack:   string(src.Stack),
	}
}

func newAdminOwner(src *Owner) adminjson.Owner {
	owner := adminjson.Owner{
		Labels:      src.Labels,
		Team:        adminjson.Team(src.Team),
		Org:         src.Org,
		Company:     src.Company,
		Contact:     adminjson.Contact(src.Contact),
		Links:       adminjson.Links(src.Links),
		Maintainers: make([]adminjson.Maintainer, len(src.Maintainers)),
	}

	if owner.Labels == nil {
		owner.Labels = map[string]string{}
	}

	for i, maintainer := range src.Maintainers {
		owner.Maintainers[i] = adminjson.Maintainer(maintainer)
	}

	return owner
}

func newAdminEvent(src *Event) adminjson.Event {
	return adminjson.Event{
		At:       src.At.UTC(),
		Report:   newAdminReport(src.Report),
		Panic:    newAdminPanic(src.Panic),
		Kind:     string(src.Kind),
		Key:      src.Key,
		Version:  src.Version,
		Language: string(src.Language),
		Storage:  src.Storage,
		Owner:    newAdminOwner(&src.Owner),
	}
}
