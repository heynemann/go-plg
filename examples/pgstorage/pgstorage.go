// Package pgstorage is a [plg.Storage] over PostgreSQL tables, built on pgx v5. Each plugin is a row of
// plg_plugins, keyed by the name of the Register call serving it (its registration) and its plugin key, with its
// files in plg_plugin_files; List returns only the enabled plugins, so disabling one and reloading
// ([plg.Container.Reload], or polling with [plg.WithPollInterval]) takes it out of service, and enabling it
// brings it back. [Migrate] creates the tables.
//
// A plugin's manifest is one of its files, plugin.yaml, which List reads with [plg.ParseManifest], as plg's own
// storages do. A plugin's version is its revision, which the database changes whenever any of its files change,
// plugin.yaml included (a trigger, see migrations/), whether through [Storage.Put] or plain SQL: a poll costs one
// small query and plg reloads exactly the plugins that changed. A version in plugin.yaml is not used.
//
// Every plugin.yaml must name the interface its plugin implements (implements:, see [plg.ParseManifest]); no
// migration can add it, as the tables do not record which interface a registration serves. List refuses a plugin
// whose plugin.yaml lacks it. Manifests stored before implements was required get it with plain SQL, one
// registration at a time:
//
//	UPDATE plg_plugin_files
//	SET content = convert_to('implements: example.com/contracts/rewards.Cohort@v1.4.0', 'UTF8')
//		|| E'\n'::bytea || content
//	WHERE registration = 'rewards' AND path = 'plugin.yaml';
package pgstorage

import (
	"context"
	_ "embed" // the migrations
	"errors"
	"fmt"
	"io"
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

// The migrations: 001 creates the plugin tables, 002 moves each plugin's manifest into a plugin.yaml file.
var (
	//go:embed migrations/001_plugins.sql
	migration001 string
	//go:embed migrations/002_manifest_file.sql
	migration002 string
)

// migration is one schema change, applied once.
type migration struct {
	version string
	script  string
}

// migrations returns the schema's migrations, oldest first. A schema change is a new file in migrations/ and a
// new entry here; applied migrations never change.
func migrations() []migration {
	return []migration{
		{version: "001_plugins.sql", script: migration001},
		{version: "002_manifest_file.sql", script: migration002},
	}
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

// manifestName is the file holding a plugin's manifest.
const manifestName = "plugin.yaml"

// listSQL selects the registration's enabled plugins with the size of their plugin.yaml (NULL without one) and
// its content when it is at most $2 bytes, so an oversized manifest never leaves the database.
const listSQL = `
	SELECT p.key, p.revision, octet_length(f.content),
		CASE WHEN octet_length(f.content) <= $2 THEN f.content END
	FROM plg_plugins p
	LEFT JOIN plg_plugin_files f ON f.registration = p.registration AND f.key = p.key AND f.path = '` +
	manifestName + `'
	WHERE p.registration = $1 AND p.enabled
	ORDER BY p.key COLLATE "C"`

// List returns the registration's enabled plugins, sorted by key, each described by its plugin.yaml
// ([plg.ParseManifest]). Their versions are "r<revision>". It fails with [plg.ErrInvalidManifest], naming the
// plugin, when one has no plugin.yaml, or one over [plg.MaxManifestSize] bytes or malformed.
func (s *Storage) List(ctx context.Context) ([]plg.Descriptor, error) {
	rows, err := s.db.Query(ctx, listSQL, s.registration, plg.MaxManifestSize)
	if err == nil {
		descs, collectErr := pgx.CollectRows(rows, scanDescriptor)
		if collectErr == nil {
			return descs, nil
		}

		err = collectErr
	}

	return nil, fmt.Errorf("pgstorage: listing %s: %w", s.registration, err)
}

var (
	errNoManifest       = errors.New("no " + manifestName)
	errManifestTooLarge = fmt.Errorf("%s is over %d bytes", manifestName, plg.MaxManifestSize)
)

// scanDescriptor reads a plugin row as List selects it and parses its manifest.
func scanDescriptor(row pgx.CollectableRow) (plg.Descriptor, error) {
	var (
		key      string
		revision int64
		size     *int64
		manifest []byte
	)

	err := row.Scan(&key, &revision, &size, &manifest)
	if err != nil {
		return plg.Descriptor{}, err //nolint:wrapcheck // List wraps it
	}

	if size == nil {
		return plg.Descriptor{}, fmt.Errorf("%w: plugin %s: %w", plg.ErrInvalidManifest, key, errNoManifest)
	}

	if *size > plg.MaxManifestSize { // its content was not selected
		return plg.Descriptor{}, fmt.Errorf("%w: plugin %s: %w", plg.ErrInvalidManifest, key, errManifestTooLarge)
	}

	desc, err := plg.ParseManifest(manifest)
	if err != nil {
		return plg.Descriptor{}, fmt.Errorf("plugin %s: %w", key, err)
	}

	desc.Key = key
	desc.Version = "r" + strconv.FormatInt(revision, 10)

	return desc, nil
}

var errFilePath = errors.New("file path is not a valid path inside the plugin")

// Open returns the plugin's files, as they are when it runs. It fails with fs.ErrNotExist when the plugin has
// none, if a path in the table is not a valid fs.FS path, and with [plg.ErrPluginTooLarge] when the files are
// over plg's limits ([plg.MaxPluginSize]): their sizes are checked in the database, in the snapshot the files
// are then read from, so an oversized plugin's content never leaves it.
func (s *Storage) Open(ctx context.Context, desc *plg.Descriptor) (fs.FS, error) {
	var files fstest.MapFS

	err := pgx.BeginFunc(ctx, s.db, func(txn pgx.Tx) error {
		var err error

		files, err = readFiles(ctx, txn, s.registration, desc.Key)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("pgstorage: opening %s/%s: %w", s.registration, desc.Key, err)
	}

	return files, nil
}

// sizesSQL selects the number of a plugin's files, the bytes of their paths and contents, and the first file over
// $3 bytes, if any. octet_length reads a stored value's size without its content.
const sizesSQL = `
	SELECT count(*), coalesce(sum(octet_length(path)), 0), coalesce(sum(octet_length(content)), 0),
		coalesce((SELECT path FROM plg_plugin_files
			WHERE registration = $1 AND key = $2 AND octet_length(content) > $3 ORDER BY path LIMIT 1), '')
	FROM plg_plugin_files WHERE registration = $1 AND key = $2`

// readFiles reads, in txn, the files of the plugin key of registration, once their sizes are within plg's limits.
func readFiles(ctx context.Context, txn pgx.Tx, registration, key string) (fstest.MapFS, error) {
	var (
		count, pathBytes, size int64
		oversized              string
	)

	// One snapshot for both queries: the files read are the files checked.
	_, err := txn.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
	if err == nil {
		err = txn.QueryRow(ctx, sizesSQL, registration, key, plg.MaxPluginFileSize).
			Scan(&count, &pathBytes, &size, &oversized)
	}

	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // Open wraps it
	case oversized != "":
		return nil, fmt.Errorf("%w: %s is over %d bytes", plg.ErrPluginTooLarge, oversized, plg.MaxPluginFileSize)
	case size > plg.MaxPluginSize:
		return nil, fmt.Errorf("%w: %d bytes, limit %d", plg.ErrPluginTooLarge, size, plg.MaxPluginSize)
	case count > plg.MaxPluginEntries:
		return nil, fmt.Errorf("%w: %d files, limit %d", plg.ErrPluginTooLarge, count, plg.MaxPluginEntries)
	case pathBytes > plg.MaxPluginPathBytes:
		return nil, fmt.Errorf("%w: paths of %d bytes, limit %d", plg.ErrPluginTooLarge, pathBytes,
			plg.MaxPluginPathBytes)
	case count == 0:
		return nil, fs.ErrNotExist
	}

	rows, err := txn.Query(ctx, "SELECT path, content FROM plg_plugin_files WHERE registration = $1 AND key = $2",
		registration, key)
	files := make(fstest.MapFS, count)

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

	return files, err //nolint:wrapcheck // Open wraps it
}

// Plugin is a plugin's row: what [Storage.Put] writes besides the files.
type Plugin struct {
	Key     string
	Enabled bool
}

// ErrNoPlugin reports a key with no plugin in the registration.
var ErrNoPlugin = errors.New("pgstorage: no such plugin")

// Put creates the plugin, or replaces it and all of its files, in one transaction, with every regular file of
// files. files must hold the plugin's plugin.yaml, which Put checks with [plg.ParseManifest] before writing
// anything (failing with [plg.ErrInvalidManifest]); the table's checks reject an empty or invalid key (".", "..",
// or one with a slash). Put always gives the plugin a new revision, so plg reloads it.
func (s *Storage) Put(ctx context.Context, plugin Plugin, files fs.FS) error {
	var (
		rows     [][]any
		read     int
		manifest []byte
	)

	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}

		content, err := readFile(files, name, read)
		read += len(content)
		rows = append(rows, []any{s.registration, plugin.Key, name, content})

		if name == manifestName {
			manifest = content
		}

		return err
	})
	if err == nil {
		err = checkManifest(manifest)
	}

	if err == nil {
		err = pgx.BeginFunc(ctx, s.db, func(txn pgx.Tx) error { return s.writePlugin(ctx, txn, plugin, rows) })
	}

	if err != nil {
		return fmt.Errorf("pgstorage: putting %s/%s: %w", s.registration, plugin.Key, err)
	}

	return nil
}

// checkManifest checks the plugin.yaml Put read, nil when there was none, as List will parse it.
func checkManifest(manifest []byte) error {
	if manifest == nil {
		return fmt.Errorf("%w: %w", plg.ErrInvalidManifest, errNoManifest)
	}

	_, err := plg.ParseManifest(manifest)

	return err //nolint:wrapcheck // Put wraps it
}

// readFile reads the file name of files, after read bytes of the plugin's other files, failing with
// [plg.ErrPluginTooLarge] when it is over plg's limits ([plg.MaxPluginSize]): by the size it states, before it
// is read, and by what it holds, read up to the limit and one byte more.
func readFile(files fs.FS, name string, read int) ([]byte, error) {
	limit := int64(min(plg.MaxPluginFileSize, plg.MaxPluginSize-read))

	file, err := files.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // Put wraps it
	}
	defer file.Close() //nolint:errcheck // the file was only read

	info, err := file.Stat()
	if err != nil {
		return nil, err //nolint:wrapcheck // Put wraps it
	}

	if info.Size() > limit {
		return nil, fileTooLarge(name)
	}

	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(content)) > limit {
		return nil, fileTooLarge(name)
	}

	return content, err //nolint:wrapcheck // Put wraps it
}

// fileTooLarge reports the file name, over [plg.MaxPluginFileSize] or bringing the plugin over [plg.MaxPluginSize].
func fileTooLarge(name string) error {
	return fmt.Errorf("%w: %s is over %d bytes or brings the plugin's files over %d", plg.ErrPluginTooLarge,
		name, plg.MaxPluginFileSize, plg.MaxPluginSize)
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
	_, err := txn.Exec(ctx, `
		INSERT INTO plg_plugins (registration, key, enabled) VALUES ($1, $2, $3)
		ON CONFLICT (registration, key) DO UPDATE SET enabled = EXCLUDED.enabled`,
		s.registration, plugin.Key, plugin.Enabled)
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
