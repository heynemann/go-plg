package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg"
)

// maxUploadBytes caps a candidate plugin's zip archive.
const maxUploadBytes = 16 << 20

// mountAdmin serves the admin endpoints on mux, each behind cfg.adminToken (an Authorization: Bearer header):
//
//   - GET /admin/plg/plugins and GET /admin/plg/plugins/{key}: plg.AdminHandler's read-only JSON, every rule's
//     status, owner, reports and panics, so whoever meets a problem knows who to talk to;
//   - POST /admin/check/{key}: dry-runs a candidate rule (plg.Check) before anyone enables it. The body is a zip
//     archive of the plugin's directory, its plugin.yaml at the root; the answer is the load's report, whether
//     the candidate would load or not.
func mountAdmin(mux *http.ServeMux, container *plg.Container, cfg config) {
	admin := func(next http.Handler) http.Handler { return requireToken(cfg.adminToken, next) }

	mux.Handle("/admin/plg/", admin(http.StripPrefix("/admin/plg", plg.AdminHandler(container))))
	mux.Handle("POST /admin/check/{key}", admin(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		answer := check(req.Context(), container, req.PathValue("key"),
			http.MaxBytesReader(writer, req.Body, maxUploadBytes))

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(answer.status)

		err := sonic.ConfigDefault.NewEncoder(writer).Encode(answer)
		if err != nil { // the client is gone
			cfg.logger.DebugContext(req.Context(), "writing a check failed", slog.Any("error", err))
		}
	})))
}

// requireToken lets through only requests bearing token.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)

	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), want) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(writer, req)
	})
}

// checkAnswer is the answer to POST /admin/check/{key}.
type checkAnswer struct {
	// Error is why the candidate would not load, or why it could not be checked; empty when it would load.
	Error   string  `json:"error"`
	Key     string  `json:"key,omitempty"`
	Version string  `json:"version,omitempty"`
	Issues  []issue `json:"issues"`
	// status is the answer's HTTP status: 200 when the candidate was checked (whether it would load or not), 400
	// for an archive or plugin.yaml that cannot be read, 503 when it could not be checked.
	status int
	// Loads says whether the candidate would load.
	Loads bool `json:"loads"`
}

// check dry-runs the zip archive in body as the rule key.
func check(ctx context.Context, container *plg.Container, key string, body io.Reader) checkAnswer {
	files, desc, err := readCandidate(body)
	if err != nil {
		return checkAnswer{Error: err.Error(), Issues: []issue{}, status: http.StatusBadRequest}
	}

	desc.Key = key

	report, err := plg.Check(ctx, container, registration, &desc, files)
	if err != nil {
		return checkAnswer{Error: err.Error(), Issues: []issue{}, status: http.StatusServiceUnavailable}
	}

	answer := checkAnswer{
		Key: report.Key, Version: report.Version, Loads: report.Err == nil, Issues: make([]issue, len(report.Issues)),
		status: http.StatusOK,
	}
	if report.Err != nil {
		answer.Error = report.Err.Error()
	}

	for i, found := range report.Issues {
		answer.Issues[i] = issue{
			Severity: string(found.Severity), Code: found.Code, Message: found.Message, Field: found.Field,
		}
	}

	return answer
}

// issue is a plg.Issue in the answer.
type issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Field    string `json:"field"`
}

var errCandidate = errors.New("invalid candidate")

// readCandidate reads a zip archive of a plugin directory and its plugin.yaml.
func readCandidate(body io.Reader) (fs.FS, plg.Descriptor, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, plg.Descriptor{}, errors.Join(errCandidate, err)
	}

	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, plg.Descriptor{}, errors.Join(errCandidate, err)
	}

	manifest, err := fs.ReadFile(archive, "plugin.yaml")
	if err != nil {
		return nil, plg.Descriptor{}, errors.Join(errCandidate, err)
	}

	desc, err := plg.ParseManifest(manifest)
	if err != nil {
		return nil, plg.Descriptor{}, err //nolint:wrapcheck // plg's error says what is wrong with the manifest
	}

	return archive, desc, nil
}
