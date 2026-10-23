package resource

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

func TestValidateProject(t *testing.T) {
	if err := ValidateProject("team-a", &v1alpha1.ProjectSpec{DisplayName: "Team A"}); err != nil {
		t.Fatalf("valid project refused: %v", err)
	}
	if err := ValidateProject("Team A", &v1alpha1.ProjectSpec{}); err == nil {
		t.Fatal("upper-case name accepted")
	}
	long := strings.Repeat("x", maxProjectText+1)
	if err := ValidateProject("team-a", &v1alpha1.ProjectSpec{Description: long}); err == nil {
		t.Fatal("over-long description accepted")
	}
}

func TestParseLimits(t *testing.T) {
	lim, err := ParseLimits(&v1alpha1.ResourceQuotaSpec{Cpu: "2", Memory: "4Gi", Storage: "100Gi", Workloads: proto.Int32(0)})
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{CPU: 2000, Memory: 4 << 30, Storage: 100 << 30, Workloads: 0}
	if lim != want {
		t.Fatalf("got %+v, want %+v", lim, want)
	}

	lim, err = ParseLimits(&v1alpha1.ResourceQuotaSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if lim != (Limits{CPU: Unlimited, Memory: Unlimited, Storage: Unlimited, Workloads: Unlimited}) {
		t.Fatalf("empty spec is not unlimited: %+v", lim)
	}

	for name, spec := range map[string]*v1alpha1.ResourceQuotaSpec{
		"bad cpu":       {Cpu: "lots"},
		"bad memory":    {Memory: "4 GB please"},
		"bad storage":   {Storage: "-1Gi"},
		"negative jobs": {Workloads: proto.Int32(-1)},
	} {
		if _, err := ParseLimits(spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if err := ValidateResourceQuota("q", spec); err == nil {
			t.Errorf("%s: ValidateResourceQuota accepted", name)
		}
	}
}

func TestUsageOf(t *testing.T) {
	w := &v1alpha1.WorkloadSpec{
		Image:     "nginx",
		Replicas:  proto.Int32(3),
		Resources: &v1alpha1.Resources{Cpu: "500m", Memory: "256Mi"},
	}
	u, err := UsageOf(w)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Usage{CPU: 1500, Memory: 3 * 256 << 20, Workloads: 1}); u != want {
		t.Fatalf("workload usage %+v, want %+v", u, want)
	}

	// Replicas unset means one replica.
	u, err = UsageOf(&v1alpha1.WorkloadSpec{Image: "nginx", Resources: &v1alpha1.Resources{Cpu: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Usage{CPU: 1000, Workloads: 1}); u != want {
		t.Fatalf("default replicas: %+v, want %+v", u, want)
	}

	u, err = UsageOf(&v1alpha1.VolumeSpec{Size: "20Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Usage{Storage: 20 << 30}); u != want {
		t.Fatalf("volume usage %+v, want %+v", u, want)
	}

	if u, _ := UsageOf(&v1alpha1.SecretSpec{}); u != (Usage{}) {
		t.Fatalf("a Secret uses %+v", u)
	}
	if _, err := UsageOf(&v1alpha1.WorkloadSpec{Resources: &v1alpha1.Resources{Memory: "lots"}}); err == nil {
		t.Fatal("bad memory quantity accepted")
	}
}

func TestCheckQuota(t *testing.T) {
	lim := Limits{CPU: 2000, Memory: 1 << 30, Storage: Unlimited, Workloads: 2}
	one := Usage{CPU: 1000, Memory: 512 << 20, Workloads: 1}
	two := one.Add(one)

	cases := []struct {
		name          string
		before, after Usage
		refused       bool
		mention       []string
	}{
		{"at the limit is accepted", one, two, false, nil},
		{"over cpu is refused", one, Usage{CPU: 2500, Memory: 512 << 20, Workloads: 1}, true, []string{"cpu", "2500m", "hard limit is 2"}},
		{"over workloads is refused", two, two.Add(one), true, []string{"workloads"}},
		{"lowering usage is never refused", Usage{CPU: 3000, Memory: 2 << 30, Workloads: 3}, one, false, nil},
		{"staying over the limit is not a new increase", Usage{CPU: 3000, Workloads: 3}, Usage{CPU: 3000, Workloads: 3}, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckQuota("team-a", "compute", lim, c.before, c.after)
			if (err != nil) != c.refused {
				t.Fatalf("refused = %v (%v), want %v", err != nil, err, c.refused)
			}
			if err == nil {
				return
			}
			for _, want := range append([]string{`quota "compute"`, `project "team-a"`}, c.mention...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

func TestCheckQuotaUnlimitedFields(t *testing.T) {
	all := Limits{CPU: Unlimited, Memory: Unlimited, Storage: Unlimited, Workloads: Unlimited}
	if err := CheckQuota("p", "q", all, Usage{}, Usage{CPU: 1 << 30, Memory: 1 << 40, Storage: 1 << 40, Workloads: 1e6}); err != nil {
		t.Fatalf("unlimited quota refused: %v", err)
	}
}

func TestShowBytes(t *testing.T) {
	for in, want := range map[int64]string{20 << 30: "20Gi", 512 << 20: "512Mi", 1000: "1000 bytes"} {
		if got := showBytes(in); got != want {
			t.Errorf("showBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
