package plg

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Status is where a plugin its registration's storages list stands; see [PluginState].
type Status string

// The statuses of a [PluginState].
const (
	// StatusServing is a plugin whose version served is the one the storages list, and whose latest on-demand
	// validation ([Validate]), if any ran, passed.
	StatusServing Status = "serving"
	// StatusDegraded is a plugin whose version served failed its latest on-demand validation ([Degrade]); it
	// serves on until a later one passes.
	StatusDegraded Status = "degraded"
	// StatusServingLastGood is a plugin whose storages list a version (or a descriptor) a reload could not load:
	// its last good version serves on.
	StatusServingLastGood Status = "serving_last_good"
	// StatusRejected is a plugin no version of which serves: the latest attempt to load what the storages list
	// failed, or listing it did (an invalid descriptor, a key listed twice, a guest not linked).
	StatusRejected Status = "rejected"
	// StatusRetired is a plugin an on-demand validation unpublished ([Unpublish]): no version serves until a
	// reload loads it again.
	StatusRetired Status = "retired"
	// StatusSkipped is a plugin in a language its Register does not enable ([WithGolangPlugins] and the others):
	// it is never loaded.
	StatusSkipped Status = "skipped"
)

// PluginState describes a plugin its registration's storages list, as [Info] and [Plugins] return it. Its reports
// and LastPanic are the container's own: treat them as read-only.
type PluginState struct {
	// LoadedAt is when the load of the version served ended (Load.At plus Load.Duration); zero when none serves.
	LoadedAt time.Time
	// Load is the report of the load that produced the version served; nil when none serves.
	Load *Report
	// Validation is the report of the latest on-demand validation ([Validate]) of the version served; nil when
	// none serves or none ran on it. The version is degraded while its Err is set.
	Validation *Report
	// Rejection is why the storages' listing is not served as listed: the report of the load (or listing) that
	// failed, for [StatusServingLastGood] and [StatusRejected], or of the on-demand validation that unpublished
	// the version, for [StatusRetired]; nil otherwise.
	Rejection *Report
	// LastPanic is the newest of the PanicErrors Panics counts; nil when there is none.
	LastPanic *PanicError
	// Key is the plugin's key, "<name>/<descriptor key>".
	Key string
	// Registration is the Register name the plugin belongs to.
	Registration string
	// Storage is the name of the storage the version served was loaded from; when none serves, of the storage
	// listing the plugin (for a key listed twice, the second one).
	Storage string
	// Language is the plugin's language: the version served's, otherwise its listing's.
	Language Language
	// Status is where the plugin stands.
	Status Status
	// Version is the version served (its content hash when its descriptor names none); empty when none serves.
	Version string
	// RejectedVersion is Rejection's Version: the version a failed load tried (empty when it failed before
	// reading the files of a descriptor naming none), or the version unpublished; empty without a Rejection.
	RejectedVersion string
	// Owner is the plugin's owner: the version served's, otherwise the one its listing names (zero when it names
	// none, or when the owner is what listing it found invalid).
	Owner Owner
	// Pool describes the instances of the version served; zero when none serves.
	Pool PoolStats
	// Panics counts the PanicErrors reported under Key (those [Container.RecentPanics] keeps, however few it
	// keeps) since the storages started listing the plugin, but those of a [Check] of a candidate with that key.
	Panics uint64
}

// PoolStats describes the instances of a plugin version ([WithPool]). Each count is read on its own, so under load
// they may be a call or an instance apart.
type PoolStats struct {
	// Min and Max are the Register's pool size bounds.
	Min, Max int
	// Instances counts the instances alive or starting.
	Instances int
	// InUse counts the instances serving at least one call.
	InUse int
	// CallsInFlight counts the calls the instances serve: more than InUse only with [WithConcurrentCalls].
	CallsInFlight int
	// IdleSlots counts the calls the instances could take now: one per idle instance, or per free lane of an
	// instance serving several calls at once ([WithConcurrentCalls]).
	IdleSlots int
	// Waiting counts the calls waiting for an instance, every one busy and the pool at its maximum.
	Waiting int
	// Refills counts the instances the background refiller started, and Grows those calls started.
	Refills, Grows uint64
	// MemoryLimit is each instance's memory limit, in bytes.
	MemoryLimit uint64
	// Memory is the linear memory of the live instances, in bytes, as last sampled: at start and after each call.
	Memory uint64
}

// Info describes the plugin with the given key, "<name>/<descriptor key>", whether a version of it serves or not
// (see [PluginState]): a plugin its registration's storages list is known once its first load settled, until a
// listing no longer names it. It fails with [ErrNotFound] for a key it does not know and with [ErrClosed] once the
// container is closed. It reads what the container keeps without waiting for loads or calls in progress, and
// runs no plugin code. It is safe for concurrent use.
func Info(ctr *Container, key string) (PluginState, error) {
	snap := ctr.catalogue.Load()
	if snap.closed {
		return PluginState{}, ErrClosed
	}

	gen := snap.plugins[key]

	var reg *registration

	if gen != nil {
		reg = gen.reg
	} else if name, _, found := strings.Cut(key, "/"); found {
		reg, _ = ctr.regs.Load(name)
	}

	if reg != nil {
		entry, _ := reg.keys.Load(key)

		state, ok := stateOf(reg, key, gen, entry)
		if ok {
			return state, nil
		}
	}

	return PluginState{}, fmt.Errorf("%w: %s", ErrNotFound, key)
}

// Plugins describes every plugin the storages of the container's registrations list, as [Info] does, in key
// order. It returns nothing once the container is closed. It is safe for concurrent use.
func Plugins(ctr *Container) []PluginState {
	states, _ := ctr.plugins()

	return states
}

// plugins is [Plugins], reporting false once the container is closed.
func (c *Container) plugins() ([]PluginState, bool) {
	snap := c.catalogue.Load()
	if snap.closed {
		return nil, false
	}

	states := make([]PluginState, 0, len(snap.plugins))

	for key, gen := range snap.plugins {
		entry, _ := gen.reg.keys.Load(key)
		state, _ := stateOf(gen.reg, key, gen, entry)
		states = append(states, state)
	}

	for _, reg := range c.regs.All() {
		for key, entry := range reg.keys.All() {
			if snap.plugins[key] != nil {
				continue // described above
			}

			if state, ok := stateOf(reg, key, nil, entry); ok {
				states = append(states, state)
			}
		}
	}

	slices.SortFunc(states, func(a, b PluginState) int { return cmp.Compare(a.Key, b.Key) })

	return states, true
}

// stateOf describes the plugin key of reg, served by gen (nil when none is), whose listing is entry (nil when
// none). It reports false when it knows nothing of it: no version serves and nothing rejected its listing, as
// while its first load runs.
func stateOf(reg *registration, key string, gen *plugin, entry *listedPlugin) (PluginState, bool) {
	rej := entry.rejectionBeside(gen)
	if gen == nil && rej == nil {
		return PluginState{}, false
	}

	state := PluginState{Key: key, Registration: reg.name}

	if entry != nil {
		state.Panics = entry.panics.Load() // before LastPanic, which is stored before the count grows
		state.LastPanic = entry.lastPanic.Load()
	}

	if rej != nil {
		state.rejected(rej)
	}

	if gen != nil {
		state.served(gen, rej)
	}

	return state, true
}

// rejected describes in s what rej, the rejection of the plugin's listing, says.
func (s *PluginState) rejected(rej *rejection) {
	s.Status, s.Rejection = rej.status, rej.report
	s.Storage, s.Language, s.Owner = rej.storage, rej.language, *rej.owner

	if rej.report != nil {
		s.RejectedVersion = rej.report.Version
	}
}

// served describes in s gen, the version served, beside rej, the rejection of the plugin's listing (nil when none).
func (s *PluginState) served(gen *plugin, rej *rejection) {
	s.Storage, s.Language, s.Owner = gen.storage, gen.desc.Language, *gen.desc.owner()
	s.Version, s.Load, s.LoadedAt = gen.desc.Version, gen.report, gen.report.At.Add(gen.report.Duration)
	s.Validation = gen.validated.Load()
	s.Pool = gen.pool.stats()

	switch {
	case s.Validation != nil && s.Validation.Err != nil:
		s.Status = StatusDegraded
	case rej != nil:
		s.Status = StatusServingLastGood
	default:
		s.Status = StatusServing
	}
}

// stats describes the pool's instances.
func (p *pool) stats() PoolStats {
	return PoolStats{
		Min:           int(p.minSize),
		Max:           int(p.maxSize),
		Instances:     int(p.size.Load()),
		InUse:         int(p.inUse.Load()),
		CallsInFlight: int(p.busy.Load()),
		IdleSlots:     len(p.idle),
		Waiting:       int(p.waiting.Load()),
		Refills:       p.refills.Load(),
		Grows:         p.grows.Load(),
		MemoryLimit:   p.limit,
		Memory:        uint64(p.memory.Load()), //nolint:gosec // G115: never negative
	}
}

// listedPlugin is a key its registration's storages list, from the listing that first names it to the first that
// no longer does: what [Info] knows of the plugin beyond the version served.
type listedPlugin struct {
	// rejection is why the listing is not served as listed; nil when it is, and while its first load runs.
	rejection atomic.Pointer[rejection]
	lastPanic atomic.Pointer[PanicError] // the newest PanicError reported under the key
	panics    atomic.Uint64              // the PanicErrors reported under the key; grows once lastPanic is stored
}

// newListedPlugin is the record of a key no listing named before.
func newListedPlugin() (*listedPlugin, bool) {
	return &listedPlugin{}, false
}

// rejection is why a listed plugin is not served as listed: a load (or listing) that failed, an on-demand
// validation that unpublished it, or a language its Register does not enable.
type rejection struct {
	report   *Report  // the failed load or listing, or the on-demand validation that unpublished it; nil when skipped
	retired  *plugin  // the version that validation unpublished; nil otherwise
	owner    *Owner   // as the listing, or the version unpublished, names it
	storage  string   // the storage listing it, or the version unpublished was loaded from
	language Language // likewise
	status   Status   // StatusRejected, StatusRetired or StatusSkipped
}

// loadRejection is the rejection of src, whose load failed as report says.
func loadRejection(src *source, report *Report) *rejection {
	return &rejection{
		report: report, owner: src.desc.owner(), storage: src.storeName, language: src.desc.Language,
		status: StatusRejected,
	}
}

// problemRejection is the rejection of a listed plugin that cannot be loaded, under key. Its owner is the
// descriptor's unless the owner is invalid.
func problemRejection(key string, p *problem) *rejection {
	owner := p.desc.owner()
	if owner.check() != nil {
		owner = &noOwner.owner
	}

	return &rejection{
		report: &Report{At: time.Now(), Err: p.err, Owner: *owner, Key: key, Version: p.desc.Version},
		owner:  owner, storage: p.storage, language: p.desc.Language, status: StatusRejected,
	}
}

// skipRejection is the rejection of src, in a language its Register does not enable.
func skipRejection(src *source) *rejection {
	return &rejection{
		owner:    src.desc.owner(),
		storage:  src.storeName,
		language: src.desc.Language,
		status:   StatusSkipped,
	}
}

// retireRejection is the rejection of gen, which an on-demand validation unpublished as report says.
func retireRejection(gen *plugin, report *Report) *rejection {
	return &rejection{
		report: report, retired: gen, owner: gen.desc.owner(), storage: gen.storage, language: gen.desc.Language,
		status: StatusRetired,
	}
}

// entry returns the record of the listed plugin key, adding it when no listing named the key before.
func (r *registration) entry(key string) *listedPlugin {
	entry, _ := r.keys.LoadOrCompute(key, newListedPlugin)

	return entry
}

// rejectionBeside returns e's rejection as it stands beside gen, the version served (nil when none is): beside a
// version served, only a failed load counts, a retirement being a race's leftover (the version an on-demand
// validation unpublished was replaced meanwhile) and a skip a reload's change not published yet. A nil e has none.
func (e *listedPlugin) rejectionBeside(gen *plugin) *rejection {
	if e == nil {
		return nil
	}

	rej := e.rejection.Load()
	if gen != nil && rej != nil && rej.status != StatusRejected {
		return nil
	}

	return rej
}

// served clears e's rejection once the version served is its listing's, unless it is the retirement of kept: an
// on-demand validation unpublished kept after the reload found it served, so no version serves.
func (e *listedPlugin) served(kept *plugin) {
	for {
		rej := e.rejection.Load()
		if rej == nil || rej.retired == kept {
			return
		}

		if e.rejection.CompareAndSwap(rej, nil) {
			return
		}
	}
}

// panicked counts failure, reported under e's key, keeping it as the newest unless a newer one is kept.
func (e *listedPlugin) panicked(failure *PanicError) {
	for {
		last := e.lastPanic.Load()
		if last != nil && last.Time.After(failure.Time) {
			break
		}

		if e.lastPanic.CompareAndSwap(last, failure) {
			break
		}
	}

	e.panics.Add(1)
}

// countPanic counts failure toward the plugin whose key it names, while that key's registration (registered or
// registering) lists it.
func (c *Container) countPanic(failure *PanicError) {
	name, _, found := strings.Cut(failure.Key, "/")
	if !found {
		return
	}

	reg, ok := c.names.Load(name)
	if !ok {
		return
	}

	if entry, listed := reg.keys.Load(failure.Key); listed {
		entry.panicked(failure)
	}
}
