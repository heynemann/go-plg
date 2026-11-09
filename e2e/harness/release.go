package harness

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/e2e/platform"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// Spec is what the campaigns command (harness/cmd/campaigns) runs: a platform on a rig's bucket and fakes, and
// campaigns. It reads it as JSON on stdin.
type Spec struct {
	// Vault resolves the plugins' secrets.
	Vault Vault
	// S3 reaches the bucket.
	S3 S3
	// Bucket holds the published artifacts.
	Bucket string
	// WalletURL is the platform's Config.WalletURL.
	WalletURL string
	// AllowedHosts are the platform's HTTP policy's allowed hosts (the vendor's).
	AllowedHosts []string
	// Campaigns run one after another.
	Campaigns []platform.Campaign
	// Users is how many users the directory holds (NewDirectory's).
	Users int
}

// S3 is where an S3 API is and its credentials.
type S3 struct {
	Endpoint, Region, AccessKey, SecretKey string
}

// Report is what the campaigns command prints as JSON: the contract module's version it was built with, its
// plugins and its campaigns' results.
type Report struct {
	// Contract is the version of ContractModule in the command's build info: "(devel)" for a directory replace.
	Contract string
	// UserFields are the JSON names of the fields of the eligibility.User it was compiled with: what the
	// version's source declares.
	UserFields []string
	// Plugins are the platform's plugins (plg.Plugins), in key order.
	Plugins []PluginReport
	// Results are the campaigns' results, in the spec's order; Errors their errors' messages ("" for none).
	Results []platform.Result
	Errors  []string
}

// PluginReport is a plugin of a Report.
type PluginReport struct {
	Key, Status, Version string
	// Issues are the codes of its load report's issues.
	Issues []string
}

// Spec returns the spec of a platform on the rig's bucket, fakes, vault and directory size, running campaigns.
func (r *Rig) Spec(tb testing.TB, campaigns ...platform.Campaign) *Spec {
	tb.Helper()

	options := r.S3.Options()
	credentials, err := options.Credentials.Retrieve(tb.Context())
	require.NoError(tb, err)

	return &Spec{
		Vault: r.Vault,
		S3: S3{
			Endpoint: aws.ToString(options.BaseEndpoint), Region: options.Region,
			AccessKey: credentials.AccessKeyID, SecretKey: credentials.SecretAccessKey,
		},
		Bucket: r.Bucket, WalletURL: r.Wallet.URL(), AllowedHosts: []string{r.Vendor.Host()},
		Campaigns: campaigns, Users: len(r.Users.Users()),
	}
}

// BuildCampaigns builds the campaigns command against the contract module's release version (ContractV1Minor), as
// a platform team builds its service against a contract release, and returns the program. The e2e module itself
// builds against the contracts' checkout (a directory replace, so its version is "(devel)" and plg cannot check
// plugins' pins against it); the command is built from this module's source with another go.mod
// (go build -modfile), which requires the release from a GOPROXY instead:
//
//   - the release is e2e/contracts with the version's overlay (harness/testdata/contracts/<version>) and its plg
//     gen adapters generated again (go generate), as the contract's authors release it;
//   - a file-based GOPROXY serves it, before the go command's module cache (testproxy.ModuleCache) for every
//     other module, into a module cache of tb, so nothing is fetched from the network and the user's module
//     cache never holds the made-up release;
//   - the build is -trimpath, so the packages compiled from tb's module cache are the go build cache's from the
//     second build on.
func BuildCampaigns(tb testing.TB, version string) string {
	tb.Helper()

	modules := tb.TempDir()
	release := filepath.Join(modules, filepath.FromSlash(ContractModule)+"@"+version)
	repository := filepath.Join(Root(tb), "..")

	copyTree(tb, filepath.Join(Root(tb), "contracts"), release)
	copyTree(tb, filepath.Join(Root(tb), "harness", "testdata", "contracts", version), release)

	// The release requires a released plg; its authors generate with this checkout's.
	goCommand(tb, release, localEnviron(), "mod", "edit", "-replace=github.com/heynemann/go-plg="+repository)
	goCommand(tb, release, localEnviron(), "generate", "./...")
	goCommand(tb, release, localEnviron(), "mod", "edit", "-dropreplace=github.com/heynemann/go-plg")

	build := tb.TempDir()
	modfile := filepath.Join(build, "go.mod")
	sum, err := os.ReadFile(filepath.Join(Root(tb), "go.sum"))
	require.NoError(tb, err)
	require.NoError(tb, os.WriteFile(filepath.Join(build, "go.sum"), sum, fileMode)) //nolint:gosec // G703: tb's

	edit := goCommand(tb, Root(tb), localEnviron(), "mod", "edit", "-print", "-dropreplace="+ContractModule,
		"-require="+ContractModule+"@"+version, "-replace=github.com/heynemann/go-plg="+repository,
		"-replace=github.com/heynemann/go-plg/examples="+filepath.Join(repository, "examples"))
	require.NoError(tb, os.WriteFile(modfile, []byte(edit), fileMode))

	program := filepath.Join(build, "campaigns")

	environ := append(testproxy.Environ(testproxy.New(tb, modules)+","+testproxy.ModuleCache(tb), tb.TempDir()),
		"GOFLAGS=-modcacherw -mod=mod")
	goCommand(tb, Root(tb), environ, "build", "-trimpath", "-modfile="+modfile, "-o", program,
		"./harness/cmd/campaigns")

	return program
}

// RunCampaigns runs the campaigns command program (BuildCampaigns') on spec and returns its report.
func RunCampaigns(tb testing.TB, program string, spec *Spec) Report {
	tb.Helper()

	input, err := sonic.Marshal(spec)
	require.NoError(tb, err)

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(tb.Context(), program)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(tb, cmd.Run(), stderr.String())

	var report Report

	require.NoError(tb, sonic.Unmarshal(stdout.Bytes(), &report), stdout.String())

	return report
}

// localEnviron is the environment of the go commands working on this checkout's modules: the process's, with
// what the go command needs resolved from its module cache only.
func localEnviron() []string {
	return append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
}

// goCommand runs the go command with args in dir and environ and returns its standard output.
func goCommand(tb testing.TB, dir string, environ []string, args ...string) string {
	tb.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(tb.Context(), "go", args...) //nolint:gosec // G204: the harness's own go commands
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(tb, cmd.Run(), "go %v: %s", args, stderr.String())

	return stdout.String()
}

// copyTree copies the files under src into dst, replacing those there.
func copyTree(tb testing.TB, src, dst string) {
	tb.Helper()

	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		rel, err := filepath.Rel(src, path)
		require.NoError(tb, err)

		data, err := os.ReadFile(path) //nolint:gosec // G304: the e2e module's own files
		require.NoError(tb, err)

		target := filepath.Join(dst, rel)
		require.NoError(tb, os.MkdirAll(filepath.Dir(target), dirMode))
		require.NoError(tb, os.WriteFile(target, data, fileMode)) //nolint:gosec // G703: under tb's directory

		return nil
	})
	require.NoError(tb, err)
}
