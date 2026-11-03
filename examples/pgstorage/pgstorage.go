// Package pgstorage is a [plg.Storage] over PostgreSQL tables, built on pgx v5. Each plugin is a row of
// plg_plugins, keyed by the name of the Register call serving it (its registration) and its plugin key, with its
// files in plg_plugin_files; List returns only the enabled plugins, so disabling one and reloading
// ([plg.Container.Reload], or polling with [plg.WithPollInterval]) takes it out of service, and enabling it
// brings it back. [Migrate] creates the tables.
//
// A plugin's version is its revision, which the database changes whenever the plugin's language, entrypoint,
// memory limit or files change (triggers, see migrations/001_plugins.sql), whether through [Storage.Put] or plain
// SQL: a poll costs one small query and plg reloads exactly the plugins that changed.
package pgstorage

import (
	"context"
	_ "embed" // the migrations
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/heynemann/go-plg"
)

// DB is the part of pgx the storage uses; *pgxpool.Pool implements it (use a pool: a Storage is used
// concurrently, a single *pgx.Conn is not safe for that).
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// migration001 creates the plugin tables.
//
//go:embed migrations/001_plugins.sql
var migration001 string

// migration is one schema change, applied once.
type migration struct {
	version string
	script  string
}

// migrations returns the schema's migrations, oldest first. A schema change is a new file in migrations/ and a
// new entry here; applied migrations never change.
func migrations() []migration {
	return []migration{{version: "001_plugins.sql", script: migration001}}
}

// setupMigrations serializes Migrate calls (several instances of a service starting together) on an advisory
// lock, held until the transaction ends (its key is "plg" in ASCII), and creates the table recording the
// migrations applied.
const setupMigrations = `
	SELECT pg_advisory_xact_lock(7367783);
	CREATE TABLE IF NOT EXISTS plg_schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	);`

// readOnly is the mode of the files Open returns.
const readOnly fs.FileMode = 0o444

// Migrate creates or updates the storage's tables in db's database. In one transaction, holding an advisory lock,
// it applies the migrations the database has not had yet (migrations/*.sql, in order), recording each in
// plg_schema_migrations; it is safe to call at every start of a service. A failing migration changes nothing.
func Migrate(ctx context.Context, db DB) error {
	err := pgx.BeginFunc(ctx, db, func(txn pgx.Tx) error { return applyMigrations(ctx, txn) })
	if err != nil {
		return fmt.Errorf("pgstorage: migrating: %w", err)
	}

	return nil
}

// applyMigrations applies, in txn, the migrations the database has not had yet.
func applyMigrations(ctx context.Context, txn pgx.Tx) error {
	_, err := txn.Exec(ctx, setupMigrations)
	if err != nil {
		return err //nolint:wrapcheck // Migrate wraps it
	}

	for _, step := range migrations() {
		tag, stepErr := txn.Exec(ctx,
			"INSERT INTO plg_schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING", step.version)
		if stepErr == nil && tag.RowsAffected() == 1 { // not applied before
			_, stepErr = txn.Exec(ctx, step.script)
		}

		if stepErr != nil {
			return fmt.Errorf("%s: %w", step.version, stepErr)
		}
	}

	return nil
}

// Storage is a [plg.Storage] over the plugins of one registration. It is safe for concurrent use.
type Storage struct {
	db           DB
	registration string
}

var _ plg.Storage = (*Storage)(nil)

// New returns the storage of the plugins of registration, the name of the Register call that serves them.
func New(db DB, registration string) *Storage {
	return &Storage{db: db, registration: registration}
}

// Name returns "postgres:<registration>".
func (s *Storage) Name() string {
	return "postgres:" + s.registration
}

// List returns the registration's enabled plugins, sorted by key. Their versions are "r<revision>".
func (s *Storage) List(ctx context.Context) ([]plg.Descriptor, error) {
	rows, err := s.db.Query(ctx, `
		SELECT key, language, entrypoint, memory_limit, revision FROM plg_plugins
		WHERE registration = $1 AND enabled
		ORDER BY key COLLATE "C"`, s.registration)
	if err == nil {
		descs, collectErr := pgx.CollectRows(rows, scanDescriptor)
		if collectErr == nil {
			return descs, nil
		}

		err = collectErr
	}

	return nil, fmt.Errorf("pgstorage: listing %s: %w", s.registration, err)
}

// scanDescriptor reads a plugin row as List selects it.
func scanDescriptor(row pgx.CollectableRow) (plg.Descriptor, error) {
	var (
		desc     plg.Descriptor
		limit    int64
		revision int64
	)

	err := row.Scan(&desc.Key, &desc.Language, &desc.Entrypoint, &limit, &revision)
	desc.MemoryLimit = uint64(limit) //nolint:gosec // G115: a CHECK keeps it from 0 to 4 GiB
	desc.Version = "r" + strconv.FormatInt(revision, 10)

	return desc, err //nolint:wrapcheck // List wraps it
}

var errFilePath = errors.New("file path is not a valid path inside the plugin")

// Open returns the plugin's files, as they are when it runs. It fails with fs.ErrNotExist when the plugin has
// none, and if a path in the table is not a valid fs.FS path.
func (s *Storage) Open(ctx context.Context, desc plg.Descriptor) (fs.FS, error) {
	rows, err := s.db.Query(ctx,
		"SELECT path, content FROM plg_plugin_files WHERE registration = $1 AND key = $2",
		s.registration, desc.Key)
	files := make(fstest.MapFS)

	var (
		name    string
		content []byte
	)

	if err == nil {
		_, err = pgx.ForEachRow(rows, []any{&name, &content}, func() error {
			if !fs.ValidPath(name) {
				return fmt.Errorf("%q: %w", name, errFilePath)
			}

			files[name] = &fstest.MapFile{Data: content, Mode: readOnly}

			return nil
		})
	}

	if err == nil && len(files) == 0 {
		err = fs.ErrNotExist
	}

	if err != nil {
		return nil, fmt.Errorf("pgstorage: opening %s/%s: %w", s.registration, desc.Key, err)
	}

	return files, nil
}

// Plugin is a plugin's row: what [Storage.Put] writes besides the files.
type Plugin struct {
	Key        string
	Language   plg.Language
	Entrypoint string
	// MemoryLimit is the plugin's per-instance memory limit in bytes, zero for the Register limit (see
	// [plg.Descriptor]).
	MemoryLimit uint64
	Enabled     bool
}

// ErrNoPlugin reports a key with no plugin in the registration.
var ErrNoPlugin = errors.New("pgstorage: no such plugin")

// maxMemoryLimit is the largest memory limit a plugin may have (the table's CHECK): 4 GiB, all a wasm32
// instance can address.
const maxMemoryLimit = 4 << 30

var errMemoryLimit = errors.New("memory limit over 4 GiB")

// Put creates the plugin, or replaces it and all of its files, in one transaction, with every regular file of
// files. The table's checks reject an empty or invalid key (".", "..", or one with a slash), an unknown language
// and an empty entrypoint. Put always gives the plugin a new revision, so plg reloads it.
func (s *Storage) Put(ctx context.Context, plugin Plugin, files fs.FS) error {
	if plugin.MemoryLimit > maxMemoryLimit {
		return fmt.Errorf("pgstorage: putting %s/%s: %w", s.registration, plugin.Key, errMemoryLimit)
	}

	var rows [][]any

	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}

		content, err := fs.ReadFile(files, name)
		rows = append(rows, []any{s.registration, plugin.Key, name, content})

		return err //nolint:wrapcheck // Put wraps it
	})
	if err == nil {
		err = pgx.BeginFunc(ctx, s.db, func(txn pgx.Tx) error { return s.writePlugin(ctx, txn, plugin, rows) })
	}

	if err != nil {
		return fmt.Errorf("pgstorage: putting %s/%s: %w", s.registration, plugin.Key, err)
	}

	return nil
}

// SetEnabled enables or disables the plugin. It fails with [ErrNoPlugin] when there is no such plugin.
func (s *Storage) SetEnabled(ctx context.Context, key string, enabled bool) error {
	return s.change(ctx, key, "UPDATE plg_plugins SET enabled = $3 WHERE registration = $1 AND key = $2", enabled)
}

// Delete removes the plugin and its files. It fails with [ErrNoPlugin] when there is no such plugin.
func (s *Storage) Delete(ctx context.Context, key string) error {
	return s.change(ctx, key, "DELETE FROM plg_plugins WHERE registration = $1 AND key = $2")
}

// writePlugin writes, in txn, the plugin's row and its files (rows of plg_plugin_files), replacing its old files.
func (s *Storage) writePlugin(ctx context.Context, txn pgx.Tx, plugin Plugin, rows [][]any) error {
	limit := int64(plugin.MemoryLimit) //nolint:gosec // G115: Put checked it is at most 4 GiB

	_, err := txn.Exec(ctx, `
		INSERT INTO plg_plugins (registration, key, language, entrypoint, memory_limit, enabled)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (registration, key) DO UPDATE SET language = EXCLUDED.language,
			entrypoint = EXCLUDED.entrypoint, memory_limit = EXCLUDED.memory_limit, enabled = EXCLUDED.enabled`,
		s.registration, plugin.Key, string(plugin.Language), plugin.Entrypoint, limit, plugin.Enabled)
	if err == nil {
		_, err = txn.Exec(ctx, "DELETE FROM plg_plugin_files WHERE registration = $1 AND key = $2",
			s.registration, plugin.Key)
	}

	if err == nil {
		_, err = txn.CopyFrom(ctx, pgx.Identifier{"plg_plugin_files"},
			[]string{"registration", "key", "path", "content"}, pgx.CopyFromRows(rows))
	}

	return err //nolint:wrapcheck // Put wraps it
}

// change runs sql, which changes the plugin key, with the registration, the key and args as its arguments.
func (s *Storage) change(ctx context.Context, key, sql string, args ...any) error {
	tag, err := s.db.Exec(ctx, sql, append([]any{s.registration, key}, args...)...)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNoPlugin
	}

	if err != nil {
		return fmt.Errorf("pgstorage: %s/%s: %w", s.registration, key, err)
	}

	return nil
}
