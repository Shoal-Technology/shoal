package appspec

import (
	"strings"
	"testing"
)

func TestImagePullSecretsParseAndExpand(t *testing.T) {
	app, err := Parse([]byte("name: blog\nimage: ghcr.io/acme/blog:1\nport: 8080\nimagePullSecrets: [ghcr-creds]\n"))
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Expand(app)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		if w := o.GetWorkload(); w != nil {
			got = w.GetSpec().GetImagePullSecrets()
		}
	}
	if len(got) != 1 || got[0] != "ghcr-creds" {
		t.Fatalf("workload imagePullSecrets = %v, want [ghcr-creds]", got)
	}
}

func TestImagePullSecretsValidation(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"imagePullSecrets: [Bad_Name]", "imagePullSecrets[0]"},
		{"imagePullSecrets: [ghcr, ghcr]", "duplicate"},
	} {
		data := "name: blog\nimage: ghcr.io/acme/blog:1\nport: 8080\n" + tc.yaml + "\n"
		if _, err := Parse([]byte(data)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) error %v, want %q", tc.yaml, err, tc.want)
		}
	}
}
