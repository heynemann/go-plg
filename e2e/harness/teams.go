package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/e2e/platform"
	"github.com/heynemann/go-plg/examples/authorrepo"
	"github.com/heynemann/go-plg/examples/s3storage"
)

// The teams of e2e/teams, each an author repository with its plg.yaml, plugins, test cases, typings and ci.sh.
const (
	// Growth owns the Go plugins: the gold cohort (paging the directory through its Host) and the min-spend
	// condition.
	Growth = "growth"
	// CRM owns the Lua plugins: the churn-risk cohort (asking the CRM vendor's API with its token) and the
	// country condition.
	CRM = "crm"
	// Payments owns the TypeScript plugins: the wallet deliverer (crediting the wallet API with its token and the
	// reward's idempotency key) and the active-buyer condition.
	Payments = "payments"
)

// The teams of e2e/teams the scenarios add to the default ones, each publishing its plugins only where a rig's
// Options name it.
const (
	// Insights owns the everyone cohort (Go: the whole directory, a page of cohort.MaxPageSize users at a time)
	// and one loyalty rule in each language: the loyal-go, loyal-lua and loyal-ts conditions.
	Insights = "insights"
	// Fraud owns conditions screening members: screening (Lua, failing for users in Mexico), velocity (Go,
	// panicking for users without orders) and watchlist (Go, carelessly printing and reporting its secret), and
	// the watched cohort (Go, putting its secret in a member ID and in cursors that loop).
	Fraud = "fraud"
	// Partners owns the pilot cohort (Lua, a fixed list) and the voucher deliverer (TypeScript, which never
	// returns from a reward of 1000 points or more).
	Partners = "partners"
	// Marketplace owns the north-america condition (TypeScript), which a scenario publishes while the platform
	// runs.
	Marketplace = "marketplace"
	// NextGen owns the batch-spend condition (Lua), written against v2.0.0 of the contracts.
	NextGen = "nextgen"
)

// Teams are the teams a rig publishes by default: the foundation's three, whose plugins every campaign of the
// smoke test uses.
func Teams() []string {
	return []string{Growth, CRM, Payments}
}

// Team is a copy of one of e2e/teams' author repositories, committed to a git repository of its own, as the
// team's own repository would be.
type Team struct {
	// Name is the team's directory under e2e/teams.
	Name string
	// Dir is the copy.
	Dir string
}

// CopyTeam copies the team name's repository into a directory of tb and commits it.
func CopyTeam(tb testing.TB, name string) *Team {
	tb.Helper()

	team := &Team{Name: name, Dir: tb.TempDir()}
	authorrepo.CopyRepository(tb, filepath.Join(Root(tb), "teams", name), team.Dir, "The "+name+" team's plugins")

	return team
}

// CI runs the team's CI script (validate, then pack) in environ (Environ's), as its CI would, and returns the
// directory of the artifacts it packed, dist/<plugin key>.plg.
func (t *Team) CI(tb testing.TB, environ []string) string {
	tb.Helper()

	return authorrepo.CI(tb, t.Dir, environ)
}

// Write writes content to the file path (slash-separated, relative to the repository), as the team's author
// would edit it; Commit records the change.
func (t *Team) Write(tb testing.TB, path, content string) {
	tb.Helper()

	file := filepath.Join(t.Dir, filepath.FromSlash(path))
	require.NoError(tb, os.MkdirAll(filepath.Dir(file), dirMode))
	require.NoError(tb, os.WriteFile(file, []byte(content), fileMode))
}

// Commit commits every change of the repository with message.
func (t *Team) Commit(tb testing.TB, message string) {
	tb.Helper()

	authorrepo.Commit(tb, t.Dir, message)
}

// registration returns the platform's registration serving a contract's interface, named by its package's last
// element and its name ("eligibility.Condition"), which is also the bucket prefix its artifacts are published
// under; false for an interface the platform does not register. A major version
// (example.com/rewards/v2/eligibility) keeps its package's name, so its artifacts go where v1's do, to be told
// apart (and skipped) by the platform.
func registration(iface string) (string, bool) {
	switch iface {
	case "cohort.Provider":
		return platform.Cohorts, true
	case "eligibility.Condition":
		return platform.Conditions, true
	case "delivery.Deliverer":
		return platform.Deliveries, true
	default:
		return "", false
	}
}

// Publish uploads every artifact of dist (a team's CI's) to bucket, as the team's CI publishes them: each under the
// prefix of the registration of the interface it implements, <registration>/<key>.plg. It returns the platform's
// keys of the plugins published ("conditions/country").
func Publish(tb testing.TB, client *s3.Client, bucket, dist string) []string {
	tb.Helper()

	artifacts, err := filepath.Glob(filepath.Join(dist, "*.plg"))
	require.NoError(tb, err)
	require.NotEmpty(tb, artifacts, "%s holds no artifact", dist)

	keys := make([]string, 0, len(artifacts))

	for _, artifact := range artifacts {
		keys = append(keys, PublishArtifact(tb, client, bucket, artifact))
	}

	return keys
}

// PublishArtifact uploads the artifact file to bucket under its registration's prefix and returns its platform key.
func PublishArtifact(tb testing.TB, client *s3.Client, bucket, file string) string {
	tb.Helper()

	data, err := os.ReadFile(file) //nolint:gosec // G304: an artifact a team's CI packed
	require.NoError(tb, err)

	art, err := plg.OpenArtifact(bytes.NewReader(data), int64(len(data)))
	require.NoError(tb, err, file)

	manifest, err := art.ReadFile("plugin.yaml")
	require.NoError(tb, err, file)

	desc, err := plg.ParseManifest(manifest)
	require.NoError(tb, err, file)

	name, _, _ := strings.Cut(desc.Implements, "@")
	pkg, iface := name[:strings.LastIndex(name, ".")], name[strings.LastIndex(name, ".")+1:]
	prefix, ok := registration(pkg[strings.LastIndex(pkg, "/")+1:] + "." + iface)
	require.True(tb, ok, "%s implements %s, which the platform does not register", file, desc.Implements)

	key := strings.TrimSuffix(filepath.Base(file), ".plg")

	_, err = client.PutObject(tb.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(prefix + "/" + key + ".plg"),
		Body:   bytes.NewReader(data),
	})
	require.NoError(tb, err)

	return prefix + "/" + key
}

// Storages returns the platform's storages of bucket: each registration's artifacts under its prefix, in the
// artifact layout of the S3 example (s3storage.WithArtifacts).
func Storages(client *s3.Client, bucket string) platform.Storages {
	storage := func(registration string) []plg.Storage {
		return []plg.Storage{s3storage.New(client, bucket, s3storage.WithPrefix(registration),
			s3storage.WithArtifacts())}
	}

	return platform.Storages{
		Cohorts:    storage(platform.Cohorts),
		Conditions: storage(platform.Conditions),
		Deliveries: storage(platform.Deliveries),
	}
}
