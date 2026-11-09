//go:build integration

package s3storage_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bytedance/sonic"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/internal/authorrepo"
	"github.com/heynemann/go-plg/examples/internal/pricingtest"
	"github.com/heynemann/go-plg/examples/pricing"
	"github.com/heynemann/go-plg/examples/s3storage"
)

// The limits examples/service passes every rule's Validate: what a rule may grant.
const (
	maxPercent = 10
	maxCents   = 1000
)

// pollInterval is how often the registration polls the bucket.
const pollInterval = 50 * time.Millisecond

// vipKey is the key the vip rule of the author repository (examples/rules) is served under.
const vipKey = pricingtest.Name + "/vip"

// isCommit matches the commit a provenance records.
const isCommit = `^[0-9a-f]{40}$`

// rulesOrder is an order every rule of examples/rules grants a discount on: ada is a gold customer (vip), SUMMER
// the seasonal coupon (seasonal) and the items three products (bundle). Its subtotal is 4000 cents.
func rulesOrder() pricing.Order {
	return pricing.Order{Customer: "ada", Coupon: "SUMMER", Items: []pricing.Item{
		{SKU: "tea", Quantity: 10, UnitCents: 200},
		{SKU: "mug", Quantity: 1, UnitCents: 1000},
		{SKU: "spoon", Quantity: 2, UnitCents: 500},
	}}
}

// rulesDiscounts returns what each rule of examples/rules grants rulesOrder, the vip rule granting vipCents.
func rulesDiscounts(vipCents int) map[string]pricing.Discount {
	return map[string]pricing.Discount{
		pricingtest.Name + "/bundle":   {Cents: 200, Reason: "3 products: 5% off"},
		pricingtest.Name + "/seasonal": {Cents: 320, Reason: "coupon SUMMER: 8% off"},
		vipKey: {
			Cents:  int64(vipCents),
			Reason: "gold customer: " + strconv.Itoa(vipCents) + " cents off",
		},
	}
}

// setVIPCents makes the vip rule of the author repository in repo grant cents off instead of 300, its test cases
// following (on an order large enough for the amount), as its author would before committing.
func setVIPCents(t *testing.T, repo string, cents int) {
	t.Helper()

	amount := strconv.Itoa(cents)

	for name, edits := range map[string][][2]string{
		"plugin.lua":     {{"local cents = 300", "local cents = " + amount}},
		"tests/vip.yaml": {{"300", amount}, {"unitCents: 450", "unitCents: 4500"}},
	} {
		data, err := os.ReadFile(filepath.Join(authorrepo.Examples(t), "rules", "vip", filepath.FromSlash(name)))
		require.NoError(t, err)

		text := string(data)

		for _, edit := range edits {
			require.Contains(t, text, edit[0], name)

			text = strings.ReplaceAll(text, edit[0], edit[1])
		}

		//nolint:gosec // G703: a path of the test's own temporary directory
		require.NoError(t, os.WriteFile(filepath.Join(repo, "vip", filepath.FromSlash(name)), []byte(text), 0o600))
	}
}

// publish uploads the artifact of key the author repository's CI packed into dist as rules/<key>.plg, as the CI
// job publishing it would, and returns it.
func publish(t *testing.T, client *s3.Client, bucket, dist, key string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dist, key+".plg")) //nolint:gosec // G304: the test's own artifact
	require.NoError(t, err)
	put(t, client, bucket, "rules/"+key+".plg", string(data))

	return string(data)
}

// vipEvents passes each loaded and rejected event of the vip rule its onEvent function (WithOnEvent's) gets to its
// channel, once per kind and version: a rejected version is tried again on every poll.
type vipEvents struct {
	seen   *xsync.Map[string, bool]
	events chan plg.Event
}

func newVIPEvents() *vipEvents {
	// More than the test publishes, so onEvent never waits.
	return &vipEvents{seen: xsync.NewMap[string, bool](), events: make(chan plg.Event, 16)}
}

func (v *vipEvents) onEvent() func(context.Context, plg.Event) {
	return func(_ context.Context, event plg.Event) {
		if event.Key != vipKey || (event.Kind != plg.EventLoaded && event.Kind != plg.EventRejected) {
			return
		}

		if _, dup := v.seen.LoadOrStore(string(event.Kind)+" "+event.Version, true); !dup {
			v.events <- event
		}
	}
}

// adminState is the part of the admin handler's JSON of a plugin this test reads.
type adminState struct {
	Rejection       *adminRejection  `json:"rejection"`
	Provenance      *adminProvenance `json:"provenance"`
	Status          string           `json:"status"`
	Version         string           `json:"version"`
	RejectedVersion string           `json:"rejectedVersion"`
}

// adminRejection is the part of a plugin's rejection in the admin JSON this test reads.
type adminRejection struct {
	Error *string `json:"error"`
}

// adminProvenance is the part of a plugin's provenance in the admin JSON this test reads.
type adminProvenance struct {
	Digest string `json:"digest"`
	Commit string `json:"commit"`
	Dirty  bool   `json:"dirty"`
}

// admin returns what the admin handler of container says of the plugin key.
func admin(t *testing.T, container *plg.Container, key string) adminState {
	t.Helper()

	recorder := httptest.NewRecorder()
	plg.AdminHandler(container).ServeHTTP(recorder,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/plugins/"+key, http.NoBody))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var state adminState

	require.NoError(t, sonic.Unmarshal(recorder.Body.Bytes(), &state))

	return state
}

// serving checks that the plugin key serves, from an artifact packed at commit from a clean tree, the version its
// provenance's digest is, and returns that version.
func serving(t *testing.T, container *plg.Container, key, commit string) string {
	t.Helper()

	state, err := plg.Info(container, key)
	require.NoError(t, err, key)
	require.NotNil(t, state.Provenance, "%s: loaded from an artifact", key)
	assert.Equal(t, plg.StatusServing, state.Status, key)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, state.Version, key)
	assert.Equal(t, state.Version, state.Provenance.Digest, "%s: versioned by its digest", key)
	assert.Equal(t, commit, state.Provenance.Commit, key)
	assert.False(t, state.Provenance.Dirty, "%s: packed from a clean tree", key)
	assert.Equal(t, plg.ContractModule{Path: "github.com/heynemann/go-plg/examples", Version: "v1.0.0"},
		state.Provenance.Contract, key)

	return state.Version
}

// The whole path from an author's commit to a served rule, through S3: the example author repository's CI script
// (examples/rules/ci.sh, running the plg command built from this checkout) validates and packs its rules, the
// artifacts are published to a bucket, and a registration with the pricing service's validation and ownership
// rules serves them from the artifact layout, polling the bucket. A newer vip rule, committed and packed again,
// goes live on the next poll with its new commit; a version failing the service's validation and a corrupt blob
// are each rejected for that rule alone, reported to its owner, while its last good version serves on.
func TestPublishedArtifactsGoLiveFromS3(t *testing.T) {
	t.Parallel()

	client := startMinIO(t)
	bucket := newBucket(t, client)

	repo := t.TempDir()
	authorrepo.Copy(t, repo)

	environ := authorrepo.Environ(t)
	dist := authorrepo.CI(t, repo, environ)

	for _, key := range []string{"bundle", "seasonal", "vip"} {
		publish(t, client, bucket, dist, key)
	}

	vip := newVIPEvents()
	container := plg.New(plg.WithOnEvent(vip.onEvent()))

	t.Cleanup(func() { require.NoError(t, container.Close(context.WithoutCancel(t.Context()))) })

	require.NoError(t, plg.Register[pricing.Rule](t.Context(), container, pricingtest.Name,
		plg.WithStorage(s3storage.New(client, bucket, s3storage.WithPrefix("rules"), s3storage.WithArtifacts())),
		plg.WithHost[pricing.Customers](pricingtest.Tiers{"ada": "gold"}),
		plg.WithValidation(func(context.Context, plg.PluginInfo) (pricing.Limits, error) {
			return pricing.Limits{MaxPercent: maxPercent, MaxCents: maxCents}, nil
		}),
		plg.WithRequiredOwnership(),
		plg.WithPool(1, 1),
		plg.WithPollInterval(pollInterval),
	))

	assert.Equal(t, rulesDiscounts(300), pricingtest.QuoteOrder(t, container, rulesOrder()), "every rule prices")

	first, err := plg.Info(container, vipKey)
	require.NoError(t, err)
	require.NotNil(t, first.Provenance)
	require.Regexp(t, isCommit, first.Provenance.Commit)

	commit := first.Provenance.Commit
	versions := map[string]string{}

	for _, key := range []string{"bundle", "seasonal", "vip"} {
		versions[key] = serving(t, container, pricingtest.Name+"/"+key, commit)
	}

	loaded := <-vip.events
	require.Equal(t, plg.EventLoaded, loaded.Kind)
	require.Equal(t, versions["vip"], loaded.Version, "Register loaded the first vip")

	// A newer vip, committed and packed again, goes live on the next poll.
	setVIPCents(t, repo, 250)
	authorrepo.Commit(t, repo, "vip: 250 cents off")
	authorrepo.CI(t, repo, environ)
	publish(t, client, bucket, dist, "vip")

	loaded = <-vip.events
	require.Equal(t, plg.EventLoaded, loaded.Kind, "the newer vip loads: %v", loaded.Report)
	assert.Equal(t, rulesDiscounts(250), pricingtest.QuoteOrder(t, container, rulesOrder()), "the newer vip prices")

	lastGood, err := plg.Info(container, vipKey)
	require.NoError(t, err)
	require.NotNil(t, lastGood.Provenance)
	assert.Regexp(t, isCommit, lastGood.Provenance.Commit)
	assert.NotEqual(t, commit, lastGood.Provenance.Commit, "packed from the new commit")
	assert.Equal(t, loaded.Version, serving(t, container, vipKey, lastGood.Provenance.Commit))
	assert.NotEqual(t, versions["vip"], loaded.Version)

	for _, key := range []string{"bundle", "seasonal"} {
		assert.Equal(t, versions[key], serving(t, container, pricingtest.Name+"/"+key, commit),
			"%s: the same artifact, the same version", key)
	}

	// A vip granting more than the service allows packs (the repository's own tests pass), and fails the
	// service's validation: rejected, reported to its owner, the last good vip serving on.
	setVIPCents(t, repo, 2000)
	authorrepo.Commit(t, repo, "vip: 2000 cents off")
	authorrepo.CI(t, repo, environ)

	invalid := publish(t, client, bucket, dist, "vip")

	rejected := <-vip.events
	require.Equal(t, plg.EventRejected, rejected.Kind)
	require.NotNil(t, rejected.Report)
	require.ErrorIs(t, rejected.Report.Err, plg.ErrBlockingIssue)
	assert.Regexp(t, `^sha256:`, rejected.Version)
	assert.NotEqual(t, lastGood.Version, rejected.Version)
	assert.Equal(t, "loyalty", rejected.Owner.Team.Name, "reported to the rule's owner")
	assert.Contains(t, rejected.Report.Issues, plg.Issue{
		Severity: plg.SeverityError, Code: "vip.cents", Message: "2000 cents off, over the 1000 cents limit",
		Field: "cents",
	})
	assert.Equal(t, rulesDiscounts(250), pricingtest.QuoteOrder(t, container, rulesOrder()), "the last good vip")

	state := admin(t, container, vipKey)
	assert.Equal(t, string(plg.StatusServingLastGood), state.Status)
	assert.Equal(t, lastGood.Version, state.Version)
	assert.Equal(t, rejected.Version, state.RejectedVersion)
	require.NotNil(t, state.Provenance)
	assert.Equal(t, lastGood.Version, state.Provenance.Digest, "the provenance of the version served")
	assert.Equal(t, lastGood.Provenance.Commit, state.Provenance.Commit)
	assert.False(t, state.Provenance.Dirty)
	require.NotNil(t, state.Rejection)
	require.NotNil(t, state.Rejection.Error)
	assert.Contains(t, *state.Rejection.Error, "vip.cents")

	// A corrupt blob in vip's place: rejected as an invalid artifact, the last good vip serving on.
	put(t, client, bucket, "rules/vip.plg", invalid[:len(invalid)/2])

	rejected = <-vip.events
	require.Equal(t, plg.EventRejected, rejected.Kind)
	require.NotNil(t, rejected.Report)
	require.ErrorIs(t, rejected.Report.Err, plg.ErrInvalidArtifact)
	assert.Regexp(t, isListingVersion, rejected.Version, "a version of the blob's own")
	assert.Equal(t, "loyalty", rejected.Owner.Team.Name, "reported to the rule's owner")
	assert.Equal(t, rulesDiscounts(250), pricingtest.QuoteOrder(t, container, rulesOrder()), "the last good vip")

	state = admin(t, container, vipKey)
	assert.Equal(t, string(plg.StatusServingLastGood), state.Status)
	assert.Equal(t, lastGood.Version, state.Version)
	assert.Equal(t, rejected.Version, state.RejectedVersion)
	require.NotNil(t, state.Provenance)
	assert.Equal(t, lastGood.Version, state.Provenance.Digest)
	require.NotNil(t, state.Rejection)
	require.NotNil(t, state.Rejection.Error)
	assert.Contains(t, *state.Rejection.Error, "invalid artifact")
}
