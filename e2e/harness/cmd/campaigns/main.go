// Command campaigns is the rewards platform as a program of its own: it reads a harness.Spec as JSON on stdin,
// starts the platform on the spec's bucket (the published artifacts, in s3storage's artifact layout), fakes and
// vault, runs the spec's campaigns one after another and prints a harness.Report as JSON on stdout.
//
// It exists to be built against a released version of the contract module (harness.BuildCampaigns), as a platform
// team's service is: the e2e tests' own binary builds against the contracts' checkout, whose version plg cannot
// check plugins' pins against.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"runtime/debug"
	"strings"

	"example.com/rewards/eligibility"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/e2e/harness"
	"github.com/heynemann/go-plg/e2e/platform"
)

func main() {
	err := run(os.Stdin, os.Stdout)
	if err != nil {
		slog.Error("campaigns", slog.Any("error", err))
		os.Exit(1)
	}
}

// run runs the spec read from in and writes its report to out. The platform, started without a context as
// platform.New is, runs until the program ends.
func run(in io.Reader, out io.Writer) error {
	ctx := context.Background()

	input, err := io.ReadAll(in)
	if err != nil {
		return err //nolint:wrapcheck // the command's own input
	}

	var spec harness.Spec

	err = sonic.Unmarshal(input, &spec)
	if err != nil {
		return err //nolint:wrapcheck // the command's own input
	}

	started, err := start(&spec)
	if err != nil {
		return err
	}

	report := harness.Report{Contract: contractVersion(), UserFields: userFields(), Plugins: plugins(started)}

	for i := range spec.Campaigns {
		result, failed := started.Run(ctx, &spec.Campaigns[i])

		message := ""
		if failed != nil {
			message = failed.Error()
		}

		report.Results = append(report.Results, result)
		report.Errors = append(report.Errors, message)
	}

	err = started.Close(ctx)
	if err != nil {
		return err //nolint:wrapcheck // the platform's own
	}

	answer, err := sonic.Marshal(report)
	if err != nil {
		return err //nolint:wrapcheck // the report encodes
	}

	_, err = out.Write(answer)

	return err //nolint:wrapcheck // stdout's
}

// start starts the platform spec describes.
func start(spec *harness.Spec) (*platform.Platform, error) {
	client := s3.New(s3.Options{
		Region:       spec.S3.Region,
		BaseEndpoint: aws.String(spec.S3.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(spec.S3.AccessKey, spec.S3.SecretKey, ""),
	})

	//nolint:wrapcheck // the platform's own
	return platform.New(&platform.Config{
		Storages:  harness.Storages(client, spec.Bucket),
		Users:     harness.NewDirectory(spec.Users),
		Secrets:   spec.Vault.Resolver(),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		WalletURL: spec.WalletURL,
		HTTP:      plg.HTTPPolicy{AllowedHosts: spec.AllowedHosts, BlockPrivateNetworks: new(false)},
		Limits:    platform.Limits{Eligibility: harness.EligibilityLimits()},
	})
}

// plugins reports the platform's plugins.
func plugins(started *platform.Platform) []harness.PluginReport {
	states := plg.Plugins(started.Container())
	reports := make([]harness.PluginReport, 0, len(states))

	for i := range states {
		state := &states[i]
		report := harness.PluginReport{Key: state.Key, Status: string(state.Status), Version: state.Version}

		if state.Load != nil {
			for _, issue := range state.Load.Issues {
				report.Issues = append(report.Issues, issue.Code)
			}
		}

		reports = append(reports, report)
	}

	return reports
}

// contractVersion returns the version of the contract module in the program's build info, "(devel)" when a
// directory replaces it.
func contractVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, dep := range info.Deps {
		switch {
		case dep.Path != harness.ContractModule:
			continue
		case dep.Replace == nil:
			return dep.Version
		case dep.Replace.Version == "":
			return "(devel)" // a directory
		default:
			return dep.Replace.Version
		}
	}

	return ""
}

// userFields returns the JSON names of eligibility.User's fields, as the program was compiled.
func userFields() []string {
	user := reflect.TypeFor[eligibility.User]()
	fields := make([]string, 0, user.NumField())

	for field := range user.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		fields = append(fields, name)
	}

	return fields
}
