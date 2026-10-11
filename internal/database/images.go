// Package database holds the pinned images of managed databases (ADR 0016,
// section 3.6). Each image is an official image pinned by its manifest digest,
// so a change of image is a reviewed change to this file.
package database

import (
	"slices"
	"strings"
)

// EnginePostgres is the Postgres engine. Redis is pass E.
const EnginePostgres = "postgres"

type pinned struct {
	engine  string
	version string // major.minor
	ref     string // official image, pinned by manifest digest
}

// images lists every supported engine version. Digests were read from the
// registry (library/postgres manifest lists) when the entry was added. Add a
// minor version by appending an entry; retire one only with a release note.
var images = []pinned{
	{EnginePostgres, "15.19", "postgres:15.19@sha256:c961aa287d8698297cb26cdfadfbe9fd2cbaf77e53cfffe9636e8d8a1e4d842c"},
	{EnginePostgres, "16.14", "postgres:16.14@sha256:95206741a5b214807675e14165369d05b93a9cf692223b616d07cca227e74b0b"},
	{EnginePostgres, "16.15", "postgres:16.15@sha256:ca0bd484cb98bf4b24eb1010e73fb3fcbd6714d240fbc1a10eea5b7dbecb641d"},
	{EnginePostgres, "17.11", "postgres:17.11@sha256:2d2b8998d31037bf721cfdf764d76ba74171b4fab3431b7f72c27c56ddbdf9e3"},
}

// Resolve maps a requested version to the pinned entry. version is either an
// exact major.minor ("16.14") or a major alone ("16"), which selects the newest
// minor of that major in the table. It returns the full major.minor, the image
// reference and whether the version is supported. The table is ordered oldest
// first, so the last match is the newest.
func Resolve(engine, version string) (full, ref string, ok bool) {
	for _, p := range images {
		if p.engine != engine {
			continue
		}
		if p.version == version || (!strings.Contains(version, ".") && Major(p.version) == version) {
			full, ref, ok = p.version, p.ref, true
		}
	}
	return full, ref, ok
}

// Image returns the pinned image reference for engine at version (see Resolve).
func Image(engine, version string) (string, bool) {
	_, ref, ok := Resolve(engine, version)
	return ref, ok
}

// Versions returns the supported versions of engine, oldest first.
func Versions(engine string) []string {
	var out []string
	for _, p := range images {
		if p.engine == engine {
			out = append(out, p.version)
		}
	}
	slices.Sort(out)
	return out
}

// Major returns the major part of a version: "16.15" gives "16".
func Major(version string) string {
	major, _, _ := strings.Cut(version, ".")
	return major
}
