package database

import (
	"regexp"
	"slices"
	"testing"
)

var digestRef = regexp.MustCompile(`^postgres:[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$`)

// Every image is an official tag pinned by a full manifest digest.
func TestImagesArePinnedByDigest(t *testing.T) {
	for _, p := range images {
		if !digestRef.MatchString(p.ref) {
			t.Errorf("%s %s: %q is not pinned by digest", p.engine, p.version, p.ref)
		}
	}
}

func TestImageLookup(t *testing.T) {
	ref, ok := Image("postgres", "16.15")
	if !ok || ref != "postgres:16.15@sha256:ca0bd484cb98bf4b24eb1010e73fb3fcbd6714d240fbc1a10eea5b7dbecb641d" {
		t.Errorf("Image(postgres, 16.15) = %q, %v", ref, ok)
	}
	for _, c := range []struct{ engine, version string }{
		{"postgres", "16.99"}, // not in the table
		{"postgres", "18"},    // no such major
		{"redis", "7.2"},      // pass E
	} {
		if _, ok := Image(c.engine, c.version); ok {
			t.Errorf("Image(%s, %s) found, want none", c.engine, c.version)
		}
	}
}

// A major resolves to its newest minor; an exact minor resolves to itself.
func TestResolveMajorToNewestMinor(t *testing.T) {
	full, ref, ok := Resolve("postgres", "16")
	if !ok || full != "16.15" || ref != "postgres:16.15@sha256:ca0bd484cb98bf4b24eb1010e73fb3fcbd6714d240fbc1a10eea5b7dbecb641d" {
		t.Errorf("Resolve(postgres, 16) = %q, %q, %v", full, ref, ok)
	}
	if full, _, ok := Resolve("postgres", "16.14"); !ok || full != "16.14" {
		t.Errorf("Resolve(postgres, 16.14) = %q, %v", full, ok)
	}
}

// The table is ordered oldest first, which Resolve relies on.
func TestTableIsOrdered(t *testing.T) {
	for i := 1; i < len(images); i++ {
		a, b := images[i-1], images[i]
		if a.engine == b.engine && a.version >= b.version {
			t.Errorf("%s %s is listed before %s, want oldest first", a.engine, a.version, b.version)
		}
	}
}

func TestVersionsAndMajor(t *testing.T) {
	got := Versions("postgres")
	want := []string{"15.19", "16.14", "16.15", "17.11"}
	if !slices.Equal(got, want) {
		t.Errorf("Versions = %v, want %v", got, want)
	}
	if m := Major("16.15"); m != "16" {
		t.Errorf("Major(16.15) = %q", m)
	}
}
