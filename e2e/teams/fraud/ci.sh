#!/bin/sh
# The CI job of this author repository: validate every plugin plg.yaml lists, keep the reports, and pack the
# valid plugins into dist/<key>.plg for the platform to publish. Run it from the repository's root with plg on
# the PATH (go install github.com/heynemann/go-plg/cmd/plg@latest), the contract module reachable through the
# go command (GOPROXY, GOPRIVATE, .netrc as for any Go build).
set -eu

mkdir -p reports

# The JSON report is for tools (its schema is versioned), the JUnit XML for the CI's test view. A failed plugin
# fails the job here, before anything is packed.
plg validate -color never -format json -junit reports/validate.xml . >reports/validate.json

# pack validates again (the same steps) and writes nothing unless every plugin is valid.
plg pack -color never -o dist .
