package resource

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

const dedupOtherSHA = "fedcba9876543210fedcba9876543210fedcba98"

func dedupBuild(name, config, ref, phase, commit string) *v1alpha1.Build {
	return &v1alpha1.Build{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec: &v1alpha1.BuildSpec{
			Config: config,
			Source: &v1alpha1.BuildConfigSpec{Git: &v1alpha1.GitSource{Ref: ref}},
		},
		Status: &v1alpha1.BuildStatus{Phase: phase, Commit: commit},
	}
}

func TestFindDuplicateBuild(t *testing.T) {
	cases := []struct {
		name   string
		builds []*v1alpha1.Build
		want   string
		found  bool
	}{
		{"no builds", nil, "", false},
		{"new build of the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", hookSHA, BuildNew, "")}, "app-a", true},
		{"pending build of the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", hookSHA, BuildPending, "")}, "app-a", true},
		{"running build of the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", hookSHA, BuildRunning, "")}, "app-a", true},
		{"complete build of the commit by ref", []*v1alpha1.Build{dedupBuild("app-a", "app", hookSHA, BuildComplete, hookSHA)}, "app-a", true},
		{"complete build resolved to the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", "main", BuildComplete, hookSHA)}, "app-a", true},
		{"failed build does not cover the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", hookSHA, BuildFailed, "")}, "", false},
		{"error build does not cover the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", hookSHA, BuildError, "")}, "", false},
		{"cancelled build does not cover the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", hookSHA, BuildCancelled, "")}, "", false},
		{"running build of another commit", []*v1alpha1.Build{dedupBuild("app-a", "app", dedupOtherSHA, BuildRunning, "")}, "", false},
		{"complete build of another commit", []*v1alpha1.Build{dedupBuild("app-a", "app", dedupOtherSHA, BuildComplete, dedupOtherSHA)}, "", false},
		{"build of another config", []*v1alpha1.Build{dedupBuild("api-a", "api", hookSHA, BuildRunning, "")}, "", false},
		{"manual build of the branch is not the commit", []*v1alpha1.Build{dedupBuild("app-a", "app", "main", BuildRunning, "")}, "", false},
		{"nil status and source are not a match", []*v1alpha1.Build{{Metadata: &v1alpha1.ObjectMeta{Name: "x"}, Spec: &v1alpha1.BuildSpec{Config: "app"}}}, "", false},
		{"the first covering build is returned", []*v1alpha1.Build{
			dedupBuild("app-old", "app", dedupOtherSHA, BuildFailed, ""),
			dedupBuild("app-new", "app", hookSHA, BuildPending, ""),
		}, "app-new", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FindDuplicateBuild("app", hookSHA, tc.builds)
			if ok != tc.found || got != tc.want {
				t.Fatalf("FindDuplicateBuild = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.found)
			}
		})
	}
}

func TestWebhookDeliveryID(t *testing.T) {
	long := make([]byte, maxDeliveryIDLen+1)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		name string
		h    http.Header
		want string
	}{
		{"none", http.Header{}, ""},
		{"github", http.Header{"X-Github-Delivery": {"guid-1"}}, "guid-1"},
		{"gitea", http.Header{"X-Gitea-Delivery": {"guid-2"}}, "guid-2"},
		{"gitlab", http.Header{"X-Gitlab-Event-Uuid": {"guid-3"}}, "guid-3"},
		{"trims spaces", http.Header{"X-Github-Delivery": {"  guid-4 "}}, "guid-4"},
		{"blank is none", http.Header{"X-Github-Delivery": {"   "}}, ""},
		{"too long is none", http.Header{"X-Github-Delivery": {string(long)}}, ""},
		{"falls through to a usable header", http.Header{"X-Github-Delivery": {string(long)}, "X-Gitlab-Event-Uuid": {"guid-5"}}, "guid-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WebhookDeliveryID(tc.h); got != tc.want {
				t.Fatalf("WebhookDeliveryID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeliveryCacheGetPut(t *testing.T) {
	c := NewDeliveryCache(4)
	if _, ok := c.Get("uid-1 d1"); ok {
		t.Fatal("empty cache returned a build")
	}
	c.Put("uid-1 d1", "app-a")
	if got, ok := c.Get("uid-1 d1"); !ok || got != "app-a" {
		t.Fatalf("Get = (%q, %v), want (app-a, true)", got, ok)
	}
	// A key is per config: the same delivery id for another config is a miss.
	if _, ok := c.Get("uid-2 d1"); ok {
		t.Fatal("delivery of another config was a hit")
	}
	// Putting an existing key replaces its build.
	c.Put("uid-1 d1", "app-b")
	if got, _ := c.Get("uid-1 d1"); got != "app-b" {
		t.Fatalf("after re-put Get = %q, want app-b", got)
	}
}

func TestDeliveryCacheIsBounded(t *testing.T) {
	const max = 3
	c := NewDeliveryCache(max)
	for i := range 10 {
		c.Put(fmt.Sprintf("d%d", i), fmt.Sprintf("app-%d", i))
	}
	if n := c.order.Len(); n != max || len(c.items) != max {
		t.Fatalf("cache holds %d/%d entries, want %d", n, len(c.items), max)
	}
	for i := range 7 {
		if _, ok := c.Get(fmt.Sprintf("d%d", i)); ok {
			t.Fatalf("d%d survived eviction", i)
		}
	}
	for i := 7; i < 10; i++ {
		if got, ok := c.Get(fmt.Sprintf("d%d", i)); !ok || got != fmt.Sprintf("app-%d", i) {
			t.Fatalf("d%d = (%q, %v), want its build", i, got, ok)
		}
	}
}

func TestDeliveryCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewDeliveryCache(2)
	c.Put("a", "app-a")
	c.Put("b", "app-b")
	// Reading a makes b the least recently used.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a missing")
	}
	c.Put("c", "app-c")
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should have been kept")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should have been kept")
	}
}

func TestDeliveryCacheMinimumSize(t *testing.T) {
	c := NewDeliveryCache(0)
	c.Put("a", "app-a")
	c.Put("b", "app-b")
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted from a cache of one")
	}
	if got, ok := c.Get("b"); !ok || got != "app-b" {
		t.Fatalf("b = (%q, %v), want app-b", got, ok)
	}
}
