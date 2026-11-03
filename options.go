package plg

import "log/slog"

// Option configures a [Container]; pass it to [New].
type Option func(*containerConfig)

type containerConfig struct {
	logger   *slog.Logger
	cacheDir string
}

// WithLogger sets the logger for loads, cache warnings and plugin failures. The default discards everything.
func WithLogger(logger *slog.Logger) Option {
	return func(c *containerConfig) {
		c.logger = logger
	}
}

// WithCompilationCacheDir sets the directory of the on-disk compilation cache shared by every guest the
// container compiles. The default is os.UserCacheDir()/plg/<wazero version>. A directory that cannot be
// created or written falls back to an in-memory cache, with a warning.
func WithCompilationCacheDir(dir string) Option {
	return func(c *containerConfig) {
		c.cacheDir = dir
	}
}

// RegisterOption configures one Register call.
type RegisterOption func(*registerConfig)

type registerConfig struct {
	storages []Storage
}

// WithStorage adds a storage whose plugins implement the registered interface. Register needs at least one;
// repeat the option to load from several.
func WithStorage(s Storage) RegisterOption {
	return func(c *registerConfig) {
		c.storages = append(c.storages, s)
	}
}
