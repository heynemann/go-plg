//go:build integration

package pgstorage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/internal/pricingtest"
	"github.com/heynemann/go-plg/examples/pgstorage"
	"github.com/heynemann/go-plg/examples/plugins"
	"github.com/heynemann/go-plg/examples/pricing"
)

const postgresImage = "postgres:17-alpine"

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// postgresServer is a PostgreSQL server in a container of the test that started it.
type postgresServer struct {
	// pool is a pool on its default database.
	pool *pgxpool.Pool
	// dsn is that database's connection string.
	dsn string
}

// startPostgres starts a PostgreSQL server for tb and its subtests; tb's cleanup terminates it, whether the test
// passes, fails or the server never became ready (RULES.md §7).
func startPostgres(tb testing.TB) *postgresServer {
	tb.Helper()

	publish := pricingtest.PublishOnFreePort(tb, "5432/tcp")

	ctr, err := postgres.Run(tb.Context(), postgresImage, postgres.BasicWaitStrategies(), publish)
	testcontainers.CleanupContainer(tb, ctr)
	require.NoError(tb, err)

	dsn, err := ctr.ConnectionString(tb.Context(), "sslmode=disable")
	require.NoError(tb, err)

	pool, err := pgxpool.New(tb.Context(), dsn)
	require.NoError(tb, err)
	tb.Cleanup(pool.Close)

	return &postgresServer{dsn: dsn, pool: pool}
}

// migratedDatabase starts a PostgreSQL server for tb and its subtests and returns a pool on its migrated default
// database.
func migratedDatabase(tb testing.TB) *pgxpool.Pool {
	tb.Helper()

	pool := startPostgres(tb).pool
	require.NoError(tb, pgstorage.Migrate(tb.Context(), pool))

	return pool
}

// newRegistration returns a registration name no other test uses.
func newRegistration() string {
	return "r" + strings.ToLower(rand.Text())
}

// newDatabase creates an empty database on the server and returns a pool on it, closed when the test ends.
func (s *postgresServer) newDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()

	name := "d" + strings.ToLower(rand.Text())

	_, err := s.pool.Exec(t.Context(), "CREATE DATABASE "+name)
	require.NoError(t, err)

	cfg, err := pgxpool.ParseConfig(s.dsn)
	require.NoError(t, err)

	cfg.ConnConfig.Database = name

	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

func files(contents ...string) fstest.MapFS {
	fsys := make(fstest.MapFS)
	for i := 0; i+1 < len(contents); i += 2 {
		fsys[contents[i]] = &fstest.MapFile{Data: []byte(contents[i+1])}
	}

	return fsys
}

func luaPlugin(key string) pgstorage.Plugin {
	return pgstorage.Plugin{Key: key, Language: plg.LanguageLua, Entrypoint: "main.lua", Enabled: true}
}

func list(t *testing.T, store *pgstorage.Storage) []plg.Descriptor {
	t.Helper()

	descs, err := store.List(t.Context())
	require.NoError(t, err)

	return descs
}

func TestMigrateIsIdempotentAndSafeConcurrently(t *testing.T) {
	t.Parallel()

	pool := startPostgres(t).pool

	var wg sync.WaitGroup

	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = pgstorage.Migrate(t.Context(), pool) })
	}

	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err, "concurrent migrations wait for each other")
	}

	require.NoError(t, pgstorage.Migrate(t.Context(), pool), "nothing left to apply")

	rows, err := pool.Query(t.Context(), "SELECT version FROM plg_schema_migrations ORDER BY version")
	require.NoError(t, err)

	var versions []string

	for rows.Next() {
		var version string

		require.NoError(t, rows.Scan(&version))

		versions = append(versions, version)
	}

	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"001_plugins.sql"}, versions, "each migration applied once")

	store := pgstorage.New(pool, "fresh")
	require.NoError(t, store.Put(t.Context(), luaPlugin("p"), files("main.lua", "")))
	assert.Len(t, list(t, store), 1, "the migrated tables work")
}

func TestMigrateFailsAsAWhole(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		existing string // what the database holds already
		failing  string // the step the error names
	}{
		"a migration fails": {
			existing: "CREATE TABLE plg_plugin_files (taken int)",
			failing:  "001_plugins.sql",
		},
		"the migrations cannot be recorded": {
			existing: "CREATE TABLE plg_schema_migrations (version int)",
			failing:  "001_plugins.sql",
		},
		"the migrations table cannot be created": {
			existing: "CREATE DOMAIN plg_schema_migrations AS int",
			failing:  `type "plg_schema_migrations" already exists`,
		},
	}

	server := startPostgres(t)

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pool := server.newDatabase(t)
			_, err := pool.Exec(t.Context(), tc.existing)
			require.NoError(t, err)

			err = pgstorage.Migrate(t.Context(), pool)
			require.ErrorContains(t, err, "pgstorage: migrating")
			require.ErrorContains(t, err, tc.failing)

			var tables int

			require.NoError(t, pool.QueryRow(t.Context(),
				"SELECT count(*) FROM pg_tables WHERE tablename = 'plg_plugins'").Scan(&tables))
			assert.Zero(t, tables, "nothing of the failed migration is left")
		})
	}
}

func TestStorageListsOnlyEnabledPlugins(t *testing.T) {
	t.Parallel()

	database := migratedDatabase(t)
	registration := newRegistration()
	store := pgstorage.New(database, registration)
	assert.Equal(t, "postgres:"+registration, store.Name())

	other := pgstorage.New(database, newRegistration())
	require.NoError(t, other.Put(t.Context(), luaPlugin("elsewhere"), files("main.lua", "")))

	limited := luaPlugin("c")
	limited.MemoryLimit = 32 << 20

	disabled := luaPlugin("b")
	disabled.Enabled = false

	for _, p := range []pgstorage.Plugin{limited, disabled, luaPlugin("a"), luaPlugin("B")} {
		require.NoError(t, store.Put(t.Context(), p, files("main.lua", "")))
	}

	descs := list(t, store)
	require.Len(t, descs, 3)

	for _, desc := range descs {
		assert.Regexp(t, `^r[0-9]+$`, desc.Version)
	}

	assert.Equal(t, []plg.Descriptor{
		{Key: "B", Language: plg.LanguageLua, Entrypoint: "main.lua", Version: descs[0].Version},
		{Key: "a", Language: plg.LanguageLua, Entrypoint: "main.lua", Version: descs[1].Version},
		{Key: "c", Language: plg.LanguageLua, Entrypoint: "main.lua", Version: descs[2].Version, MemoryLimit: 32 << 20},
	}, descs, "the enabled plugins of this registration, in byte order")

	require.NoError(t, store.SetEnabled(t.Context(), "b", true))
	require.NoError(t, store.SetEnabled(t.Context(), "a", false))

	keys := make([]string, 0, len(descs))
	for _, desc := range list(t, store) {
		keys = append(keys, desc.Key)
	}

	assert.Equal(t, []string{"B", "b", "c"}, keys)
}

func TestStorageVersionsFollowTheRows(t *testing.T) {
	t.Parallel()

	database := migratedDatabase(t)
	registration := newRegistration()
	store := pgstorage.New(database, registration)
	require.NoError(t, store.Put(t.Context(), luaPlugin("p"), files("main.lua", "return 1", "lib/x.lua", "")))
	require.NoError(t, store.Put(t.Context(), luaPlugin("q"), files("main.lua", "")))

	version := func() string {
		t.Helper()

		descs := list(t, store)
		require.NotEmpty(t, descs)
		require.Equal(t, "p", descs[0].Key)

		return descs[0].Version
	}

	sql := func(statement string) func() {
		return func() {
			_, err := database.Exec(t.Context(), statement, registration)
			require.NoError(t, err)
		}
	}

	first := version()
	seen := map[string]string{first: "the first Put"}
	assert.Equal(t, first, version(), "stable while nothing changes")

	changes := []struct {
		change func()
		name   string
	}{
		{name: "a file edited", change: sql(
			`UPDATE plg_plugin_files SET content = 'return 2' WHERE registration = $1 AND key = 'p' AND path = 'main.lua'`,
		)},
		{name: "a file added", change: sql(
			`INSERT INTO plg_plugin_files VALUES ($1, 'p', 'lib/y.lua', '')`)},
		{name: "a file removed", change: sql(
			`DELETE FROM plg_plugin_files WHERE registration = $1 AND key = 'p' AND path = 'lib/x.lua'`)},
		{name: "the entrypoint changed", change: sql(
			`UPDATE plg_plugins SET entrypoint = 'lib/y.lua' WHERE registration = $1 AND key = 'p'`)},
		{name: "the memory limit changed", change: sql(
			`UPDATE plg_plugins SET memory_limit = 65536 WHERE registration = $1 AND key = 'p'`)},
		{name: "put again, unchanged", change: func() {
			require.NoError(t, store.Put(t.Context(), luaPlugin("p"), files("main.lua", "")))
		}},
		{name: "deleted and put again", change: func() {
			require.NoError(t, store.Delete(t.Context(), "p"))
			require.NoError(t, store.Put(t.Context(), luaPlugin("p"), files("main.lua", "")))
		}},
	}

	for _, step := range changes {
		step.change()

		got := version()
		assert.NotContains(t, seen, got, "%s: a version never seen before", step.name)

		seen[got] = step.name
	}

	before := version()

	sql(`UPDATE plg_plugins SET entrypoint = entrypoint, enabled = false WHERE registration = $1 AND key = 'q'`)()
	sql(`UPDATE plg_plugins SET enabled = true WHERE registration = $1 AND key = 'q'`)()
	sql(`UPDATE plg_plugins SET enabled = enabled WHERE registration = $1 AND key = 'p'`)()
	assert.Equal(t, before, version(), "enabling, disabling and rewriting the same values change nothing")
}

func TestStorageOpen(t *testing.T) {
	t.Parallel()

	database := migratedDatabase(t)
	registration := newRegistration()
	store := pgstorage.New(database, registration)

	want := files("main.lua", "return require('lib.x')", "lib/x.lua", "return 1", "README", "binary\x00\xff")
	disabled := luaPlugin("p")
	disabled.Enabled = false
	require.NoError(t, store.Put(t.Context(), disabled, want))

	got, err := store.Open(t.Context(), plg.Descriptor{Key: "p"})
	require.NoError(t, err, "a disabled plugin still opens: a version being served may be disabled meanwhile")
	require.NoError(t, fstest.TestFS(got, "main.lua", "lib/x.lua", "README"))

	for name, file := range want {
		data, readErr := fs.ReadFile(got, name)
		require.NoError(t, readErr)
		assert.Equal(t, file.Data, data, name)
	}

	_, err = store.Open(t.Context(), plg.Descriptor{Key: "none"})
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = database.Exec(t.Context(), `INSERT INTO plg_plugin_files VALUES ($1, 'p', 'a//b.lua', '')`, registration)
	require.NoError(t, err)

	_, err = store.Open(t.Context(), plg.Descriptor{Key: "p"})
	require.ErrorContains(t, err, `"a//b.lua"`, "a path an fs.FS cannot hold")
}

func TestStorageRefusesInvalidPlugins(t *testing.T) {
	t.Parallel()

	store := pgstorage.New(migratedDatabase(t), newRegistration())

	tests := map[string]pgstorage.Plugin{
		"empty key":         {Language: plg.LanguageLua, Entrypoint: "main.lua"},
		"key with a slash":  {Key: "a/b", Language: plg.LanguageLua, Entrypoint: "main.lua"},
		"dot key":           {Key: ".", Language: plg.LanguageLua, Entrypoint: "main.lua"},
		"dot-dot key":       {Key: "..", Language: plg.LanguageLua, Entrypoint: "main.lua"},
		"unknown language":  {Key: "p", Language: "python", Entrypoint: "main.py"},
		"no entrypoint":     {Key: "p", Language: plg.LanguageLua},
		"memory over 4 GiB": {Key: "p", Language: plg.LanguageLua, Entrypoint: "main.lua", MemoryLimit: 4<<30 + 1},
	}

	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := store.Put(t.Context(), p, files("main.lua", ""))
			require.Error(t, err)
			assert.ErrorContains(t, err, "pgstorage: putting")
		})
	}

	err := store.Put(t.Context(), luaPlugin("p"), os.DirFS(filepath.Join(t.TempDir(), "none")))
	require.ErrorIs(t, err, fs.ErrNotExist, "files that cannot be read")

	at4GiB := luaPlugin("p")
	at4GiB.MemoryLimit = 4 << 30
	require.NoError(t, store.Put(t.Context(), at4GiB, files("main.lua", "")), "4 GiB is the limit")

	err = store.Put(
		t.Context(),
		pgstorage.Plugin{Key: "p", Language: "python", Entrypoint: "x"},
		files("main.lua", "-"),
	)
	require.Error(t, err)

	descs := list(t, store)
	require.Len(t, descs, 1)
	assert.Equal(t, plg.LanguageLua, descs[0].Language, "a failed Put changes nothing")

	got, err := store.Open(t.Context(), descs[0])
	require.NoError(t, err)

	data, err := fs.ReadFile(got, "main.lua")
	require.NoError(t, err)
	assert.Empty(t, data, "the files of a failed Put are not kept")

	require.ErrorIs(t, store.SetEnabled(t.Context(), "none", true), pgstorage.ErrNoPlugin)
	require.ErrorIs(t, store.Delete(t.Context(), "none"), pgstorage.ErrNoPlugin)

	require.NoError(t, store.Delete(t.Context(), "p"))
	assert.Empty(t, list(t, store))

	_, err = store.Open(t.Context(), descs[0])
	require.ErrorIs(t, err, fs.ErrNotExist, "its files went with it")
}

// countingDB is a pgstorage.DB counting the bytes of the values its queries return, in transactions too.
type countingDB struct {
	*pgxpool.Pool

	received *atomic.Int64
}

//nolint:ireturn // it implements pgstorage.DB, whose Begin returns pgx.Tx
func (db countingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	txn, err := db.Pool.Begin(ctx)

	return countingTx{Tx: txn, received: db.received}, err //nolint:wrapcheck // a transparent wrapper
}

//nolint:ireturn // it implements pgstorage.DB, whose Query returns pgx.Rows
func (db countingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := db.Pool.Query(ctx, sql, args...)

	return countingRows{Rows: rows, received: db.received}, err //nolint:wrapcheck // a transparent wrapper
}

type countingTx struct {
	pgx.Tx

	received *atomic.Int64
}

//nolint:ireturn // it implements pgx.Tx, whose Query returns pgx.Rows
func (txn countingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := txn.Tx.Query(ctx, sql, args...)

	return countingRows{Rows: rows, received: txn.received}, err //nolint:wrapcheck // a transparent wrapper
}

type countingRows struct {
	pgx.Rows

	received *atomic.Int64
}

func (rows countingRows) Next() bool {
	next := rows.Rows.Next()
	if next {
		for _, value := range rows.RawValues() {
			rows.received.Add(int64(len(value)))
		}
	}

	return next
}

// TestStorageOpenRefusesOversizedPluginsBeforeReadingThem: a plugin with a file over plg.MaxPluginFileSize, or
// files over plg.MaxPluginSize together, fails to open with plg.ErrPluginTooLarge before its content leaves the
// database, and Put refuses such files.
func TestStorageOpenRefusesOversizedPluginsBeforeReadingThem(t *testing.T) {
	t.Parallel()

	database := migratedDatabase(t)
	registration := newRegistration()
	received := new(atomic.Int64)
	store := pgstorage.New(countingDB{Pool: database, received: received}, registration)

	// The content is made in the database: the test transfers none of it either.
	insert := func(key string, sizes ...int) {
		t.Helper()

		require.NoError(t, store.Put(t.Context(), luaPlugin(key), files("main.lua", "")))

		for i, size := range sizes {
			_, err := database.Exec(t.Context(), `INSERT INTO plg_plugin_files
				VALUES ($1, $2, $3, convert_to(repeat('x', $4), 'UTF8'))`,
				registration, key, "f"+strconv.Itoa(i), size)
			require.NoError(t, err)
		}
	}

	insert("file", plg.MaxPluginFileSize+1)
	insert("total", plg.MaxPluginFileSize, plg.MaxPluginFileSize, plg.MaxPluginFileSize, plg.MaxPluginFileSize, 1)
	insert("fits", plg.MaxPluginFileSize)

	// Many files, and long paths (a path index entry holds at most ~2.7 KB, so many paths of ~2,000 bytes).
	for key, sql := range map[string]string{
		"entries": `INSERT INTO plg_plugin_files SELECT $1, 'entries', 'e' || i, '' FROM generate_series(1, $2) i`,
		"paths": `INSERT INTO plg_plugin_files
			SELECT $1, 'paths', repeat('p', 1990) || i, '' FROM generate_series(1, $2 / 1900 + 1) i`,
	} {
		limit := plg.MaxPluginEntries
		if key == "paths" {
			limit = plg.MaxPluginPathBytes
		}

		require.NoError(t, store.Put(t.Context(), luaPlugin(key), files("main.lua", "")))

		_, err := database.Exec(t.Context(), sql, registration, limit)
		require.NoError(t, err)
	}

	for _, key := range []string{"file", "total", "entries", "paths"} {
		received.Store(0)

		_, err := store.Open(t.Context(), plg.Descriptor{Key: key})
		require.ErrorIs(t, err, plg.ErrPluginTooLarge, key)
		assert.Less(t, received.Load(), int64(1<<10), "%s: no content was transferred", key)
	}

	got, err := store.Open(t.Context(), plg.Descriptor{Key: "fits"})
	require.NoError(t, err, "the limits themselves are allowed")

	data, err := fs.ReadFile(got, "f0")
	require.NoError(t, err)
	assert.Len(t, data, plg.MaxPluginFileSize)

	big := fstest.MapFS{"main.lua": {Data: make([]byte, plg.MaxPluginFileSize+1)}}
	require.ErrorIs(t, store.Put(t.Context(), luaPlugin("put"), big), plg.ErrPluginTooLarge, "a file too large")

	many := make(fstest.MapFS)
	chunk := make([]byte, plg.MaxPluginFileSize)

	for i := range plg.MaxPluginSize/plg.MaxPluginFileSize + 1 {
		many["f"+strconv.Itoa(i)] = &fstest.MapFile{Data: chunk}
	}

	require.ErrorIs(t, store.Put(t.Context(), luaPlugin("put"), many), plg.ErrPluginTooLarge, "files too large")

	err = store.Put(t.Context(), luaPlugin("put"), lyingFS{"main.lua": {}})
	require.ErrorIs(t, err, plg.ErrPluginTooLarge, "a file holding more than it states is read up to the limit")

	for name, want := range map[string]error{"unopenable": fs.ErrPermission, "unstatable": fs.ErrInvalid} {
		require.ErrorIs(t, store.Put(t.Context(), luaPlugin("put"), lyingFS{name: {}}), want, name)
	}

	assert.Len(t, list(t, store), 5, "the refused plugin was not written")
}

// lyingFS is a MapFS whose files misbehave: "unopenable" fails to open, "unstatable" to stat, and any other
// states 0 bytes but holds endless zeros.
type lyingFS fstest.MapFS

func (fsys lyingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fstest.MapFS(fsys).ReadDir(name) //nolint:wrapcheck // a transparent wrapper
}

func (fsys lyingFS) Open(name string) (fs.File, error) {
	if name == "unopenable" {
		return nil, fs.ErrPermission
	}

	file, err := fstest.MapFS(fsys).Open(name)
	if err != nil || name == "." {
		return file, err //nolint:wrapcheck // a transparent wrapper
	}

	return lyingFile{File: file, unstatable: name == "unstatable"}, nil
}

type lyingFile struct {
	fs.File

	unstatable bool
}

func (f lyingFile) Stat() (fs.FileInfo, error) {
	if f.unstatable {
		return nil, fs.ErrInvalid
	}

	return f.File.Stat() //nolint:wrapcheck // a transparent wrapper
}

func (lyingFile) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

func TestStorageReportsDatabaseFailures(t *testing.T) {
	t.Parallel()

	pool := startPostgres(t).pool
	store := pgstorage.New(pool, "r")

	_, err := store.List(t.Context())
	require.ErrorContains(t, err, "pgstorage: listing r", "no tables yet")

	_, err = store.Open(t.Context(), plg.Descriptor{Key: "p"})
	require.ErrorContains(t, err, "pgstorage: opening r/p")

	require.ErrorContains(t, store.SetEnabled(t.Context(), "p", true), "pgstorage: r/p")

	pool.Close()

	require.ErrorContains(t, pgstorage.Migrate(t.Context(), pool), "pgstorage: migrating", "a closed pool")
	require.ErrorContains(t, store.Put(t.Context(), luaPlugin("p"), files("main.lua", "")), "pgstorage: putting r/p")
}

// putPlugins puts every plugin directory of fsys, described by its plugin.json, into the storage.
func putPlugins(t *testing.T, store *pgstorage.Storage, fsys fs.FS) {
	t.Helper()

	dirs, err := fs.ReadDir(fsys, ".")
	require.NoError(t, err)

	for _, dir := range dirs {
		manifest, err := fs.ReadFile(fsys, dir.Name()+"/plugin.json")
		require.NoError(t, err)

		desc, err := plg.ParseManifest(manifest)
		require.NoError(t, err)

		sub, err := fs.Sub(fsys, dir.Name())
		require.NoError(t, err)

		p := pgstorage.Plugin{
			Key: dir.Name(), Language: desc.Language, Entrypoint: desc.Entrypoint, MemoryLimit: desc.MemoryLimit,
			Enabled: true,
		}
		require.NoError(t, store.Put(t.Context(), p, sub))
	}
}

// The storage serves plg: the example rules in Go, Lua and TypeScript load from PostgreSQL, and reloads follow
// the table, disabled plugins included.
func TestStorageServesThePricingRules(t *testing.T) {
	t.Parallel()

	store := pgstorage.New(migratedDatabase(t), newRegistration())
	putPlugins(t, store, plugins.FS)

	container := pricingtest.Register(t, store)
	assert.Equal(t, pricingtest.Discounts(), pricingtest.Quote(t, container))

	require.NoError(t, store.SetEnabled(t.Context(), "loyalty", false))

	coupon, err := fs.Sub(plugins.FS, "coupon")
	require.NoError(t, err)

	coupons, err := fs.ReadFile(coupon, "coupons.ts")
	require.NoError(t, err)

	changed := fstest.MapFS{"coupons.ts": {Data: bytes.Replace(coupons, []byte("500"), []byte("700"), 1)}}

	for _, name := range []string{"plugin.json", "rule.ts"} {
		data, readErr := fs.ReadFile(coupon, name)
		require.NoError(t, readErr)

		changed[name] = &fstest.MapFile{Data: data}
	}

	ts := pgstorage.Plugin{Key: "coupon", Language: plg.LanguageTypeScript, Entrypoint: "rule.ts", Enabled: true}
	require.NoError(t, store.Put(t.Context(), ts, changed))
	require.NoError(t, container.Reload(t.Context()))

	want := pricingtest.Discounts()
	want["pricing/coupon"] = pricing.Discount{Cents: 700, Reason: "coupon WELCOME5"}
	delete(want, "pricing/loyalty")
	assert.Equal(t, want, pricingtest.Quote(t, container), "the changed rule reloaded, the disabled one out of service")

	require.NoError(t, store.SetEnabled(t.Context(), "loyalty", true))
	require.NoError(t, container.Reload(t.Context()))

	want["pricing/loyalty"] = pricingtest.Discounts()["pricing/loyalty"]
	assert.Equal(t, want, pricingtest.Quote(t, container), "enabled again")
}
