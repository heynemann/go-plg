//go:build integration

package contractref_test

import (
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/heynemann/go-plg/internal/contractref"
)

// TestModuleOfReadsRealBuilds builds a service against a contract module, served by a file-based module proxy
// (offline), three ways: at the version it requires, with a replace directive pointing at another version, and with
// one pointing at a local checkout. Each binary's build info gives the version the service was built with: the
// required one, the replacement's, and (devel) with the checkout's directory. The service's own package is in its
// main module, (devel) as well.
func TestModuleOfReadsRealBuilds(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	contracts := filepath.Join(root, "contracts")
	writeTree(t, contracts, map[string]string{
		"go.mod":             "module example.com/contracts\n\ngo 1.27\n",
		"rewards/rewards.go": "package rewards\n\n// Cohort says who belongs to a cohort.\ntype Cohort interface{ IsMember(id string) bool }\n",
	})

	proxy := filepath.Join(root, "proxy")
	for _, version := range []string{"v1.3.0", "v1.4.0"} {
		publish(t, proxy, contracts, module.Version{Path: "example.com/contracts", Version: version})
	}

	modcache := filepath.Join(root, "modcache")

	for name, tc := range map[string]struct {
		replace string
		want    contractref.Module
	}{
		"required version": {want: contractref.Module{Path: "example.com/contracts", Version: "v1.4.0"}},
		"replaced by another version": {
			replace: "example.com/contracts => example.com/contracts v1.3.0",
			want:    contractref.Module{Path: "example.com/contracts", Version: "v1.3.0"},
		},
		"replaced by a local checkout": {
			replace: "example.com/contracts => ../contracts",
			want:    contractref.Module{Path: "example.com/contracts", Version: contractref.Devel, Dir: "../contracts"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gomod := "module example.com/service\n\ngo 1.27\n\nrequire example.com/contracts v1.4.0\n"
			if tc.replace != "" {
				gomod += "\nreplace " + tc.replace + "\n"
			}

			service := filepath.Join(root, strings.ReplaceAll(name, " ", "-"))
			writeTree(t, service, map[string]string{
				"go.mod": gomod,
				"main.go": "package main\n\nimport \"example.com/contracts/rewards\"\n\nvar _ rewards.Cohort\n\n" +
					"func main() {}\n",
			})

			bin := filepath.Join(service, "service")
			//nolint:gosec // G204: the go command building the test's own service
			build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".")
			build.Dir = service

			build.Env = append(os.Environ(), "GOPROXY=file://"+filepath.ToSlash(proxy), "GOFLAGS=-mod=mod -modcacherw",
				"GOSUMDB=off", "GONOSUMDB=", "GOPRIVATE=", "GONOPROXY=", "GOWORK=off", "GOTOOLCHAIN=local",
				"GOMODCACHE="+modcache)
			out, err := build.CombinedOutput()
			require.NoError(t, err, "%s", out)

			bi, err := buildinfo.ReadFile(bin)
			require.NoError(t, err)

			got, found := contractref.ModuleOf(bi, "example.com/contracts/rewards")
			require.True(t, found)
			assert.Equal(t, tc.want, got)

			main, found := contractref.ModuleOf(bi, "example.com/service")
			require.True(t, found)
			assert.Equal(t, contractref.Module{Path: "example.com/service", Version: contractref.Devel}, main)
		})
	}
}

// writeTree writes files (slash-separated path -> content) under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

// publish adds the module in dir to the file-based module proxy at proxy, at mod's version.
func publish(t *testing.T, proxy, dir string, mod module.Version) {
	t.Helper()

	escaped, err := module.EscapePath(mod.Path)
	require.NoError(t, err)

	versions := filepath.Join(proxy, filepath.FromSlash(escaped), "@v")
	require.NoError(t, os.MkdirAll(versions, 0o750))

	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: the test's own temp tree
	require.NoError(t, err)
	//nolint:gosec // G703: the test's own temp tree
	require.NoError(t, os.WriteFile(filepath.Join(versions, mod.Version+".mod"), gomod, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(versions, mod.Version+".info"),
		[]byte(`{"Version":"`+mod.Version+`"}`), 0o600))

	//nolint:gosec // G304: the test's own temp tree
	archive, err := os.Create(filepath.Join(versions, mod.Version+".zip"))
	require.NoError(t, err)
	require.NoError(t, modzip.CreateFromDir(archive, mod, dir))
	require.NoError(t, archive.Close())

	//nolint:gosec // G304: the test's own temp tree
	list, err := os.OpenFile(filepath.Join(versions, "list"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)

	_, err = list.WriteString(mod.Version + "\n")
	require.NoError(t, err)
	require.NoError(t, list.Close())
}
