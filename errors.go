package plg

import (
	"errors"

	"github.com/heynemann/go-plg/internal/httpcap"
)

// Errors returned by plg. Test for them with [errors.Is]; the returned errors wrap them with details such as the
// key, the method or the offending type.
var (
	// ErrNotFound reports a key with no plugin behind it (never registered, or removed by a reload), or a name
	// no interface is registered under.
	ErrNotFound = errors.New("plg: plugin not found")
	// ErrTypeMismatch reports a key whose plugin was registered for another interface.
	ErrTypeMismatch = errors.New("plg: plugin registered for another interface")
	// ErrClosed reports a use of a closed [Container], or of a plugin it served.
	ErrClosed = errors.New("plg: container closed")
	// ErrInvalidInterface reports an interface that breaks the interface contract: every method takes a
	// context.Context first, returns an error last and uses only data types.
	ErrInvalidInterface = errors.New("plg: invalid interface")
	// ErrInvalidHost reports a [WithHost] option Register cannot accept: its interface breaks the interface
	// contract or is declared outside the registered interface's package, its implementation is nil, or the
	// option is given twice.
	ErrInvalidHost = errors.New("plg: invalid host")
	// ErrInvalidContract reports a [Contract] that [RegisterContract] cannot accept.
	ErrInvalidContract = errors.New("plg: invalid contract")
	// ErrNoContract reports an interface without a registered [Contract].
	ErrNoContract = errors.New("plg: no contract registered")
	// ErrInvalidName reports a Register name that is empty or contains a slash.
	ErrInvalidName = errors.New("plg: invalid name")
	// ErrDuplicateName reports a name registered twice in one container.
	ErrDuplicateName = errors.New("plg: name already registered")
	// ErrNoStorage reports a Register call without any storage.
	ErrNoStorage = errors.New("plg: no storage")
	// ErrInvalidOption reports a Register option with an invalid value, such as a [WithPool] minimum above its
	// maximum.
	ErrInvalidOption = errors.New("plg: invalid option")
	// ErrMemoryBudget reports a Register whose plugins' worst-case memory, added to that of the plugins already
	// registered, exceeds the container's [WithMemoryBudget].
	ErrMemoryBudget = errors.New("plg: memory budget exceeded")
	// ErrInvalidDescriptor reports a [Descriptor] without a key or an entrypoint, or whose MemoryLimit is under
	// one 64 KiB page or over 4 GiB.
	ErrInvalidDescriptor = errors.New("plg: invalid descriptor")
	// ErrInvalidManifest reports a plugin directory of an [FSStorage] whose plugin.json is missing or malformed.
	ErrInvalidManifest = errors.New("plg: invalid plugin.json")
	// ErrDuplicateKey reports a plugin key listed twice by the storages of one Register call.
	ErrDuplicateKey = errors.New("plg: duplicate plugin key")
	// ErrUnsupportedLanguage reports a plugin in a language plg cannot run.
	ErrUnsupportedLanguage = errors.New("plg: unsupported language")
	// ErrGuestNotLinked reports a plugin whose language guest package is not imported by the program.
	ErrGuestNotLinked = errors.New("plg: guest not linked")
	// ErrLoad reports a plugin that failed to load: its storage could not open it, its source does not
	// compile, or its Create failed.
	ErrLoad = errors.New("plg: plugin failed to load")
	// ErrGoStatement reports a Go plugin whose source contains go statements; the message names the file, line
	// and column of each. Register checks the source before loading it: goroutines inside a WebAssembly guest
	// give no parallelism and are paused between calls, so plugins get their parallelism from the instance
	// pool instead.
	ErrGoStatement = errors.New("plg: go statements are not allowed in plugins")
	// ErrInvalidHTTPPolicy reports an [HTTPPolicy] Register cannot accept.
	ErrInvalidHTTPPolicy = httpcap.ErrInvalidPolicy
	// ErrCall reports a call that failed outside the plugin's own code: the guest trapped, panicked, was
	// stopped by its context or broke the guest ABI, or the host panicked serving it. The error is a
	// [*PanicError] in [PhaseCall]; the instance that served it is discarded.
	ErrCall = errors.New("plg: plugin call failed")
	// ErrConversion reports an argument or result that cannot cross the plugin boundary, such as an integer
	// beyond ±2^53 (which Lua and JavaScript numbers cannot represent exactly) or a pointer cycle. The message
	// names the parameter or result and the field inside it. The instance stays usable.
	ErrConversion = errors.New("plg: value cannot cross the plugin boundary")
)

// PluginError is an error a plugin method returned. Only its message crosses the sandbox boundary, so Error
// returns exactly the plugin's message, as an in-process implementation would.
type PluginError struct {
	// Key is the plugin key, "<name>/<descriptor key>".
	Key string
	// Method is the interface method that returned the error.
	Method string
	// Message is the plugin error's message.
	Message string
}

// Error returns the plugin's message.
func (e *PluginError) Error() string {
	return e.Message
}
