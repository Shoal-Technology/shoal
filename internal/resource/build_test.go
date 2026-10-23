package resource

import (
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// validBuildConfig returns a BuildConfig spec with defaults applied, as the API
// stores it.
func validBuildConfig(name string) *v1alpha1.BuildConfigSpec {
	s := &v1alpha1.BuildConfigSpec{Git: &v1alpha1.GitSource{Url: "https://github.com/acme/web.git"}}
	DefaultBuildConfig(name, s)
	return s
}

func TestDefaultBuildConfig(t *testing.T) {
	s := validBuildConfig("web")
	g := s.GetGit()
	if g.GetRef() != DefaultGitRef || g.GetContextDir() != DefaultContextDir || g.GetDockerfile() != DefaultDockerfile {
		t.Fatalf("git defaults: %+v", g)
	}
	if s.GetOutput().GetRepository() != "web" || s.GetOutput().GetTag() != DefaultTagTemplate {
		t.Fatalf("output defaults: %+v", s.GetOutput())
	}
}

func TestValidateBuildConfigAccepts(t *testing.T) {
	cases := map[string]func(*v1alpha1.BuildConfigSpec){
		"https":      func(*v1alpha1.BuildConfigSpec) {},
		"ssh scheme": func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "ssh://git@github.com/acme/web.git" },
		"scp form":   func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "git@github.com:acme/web.git" },
		"ref and dirs": func(s *v1alpha1.BuildConfigSpec) {
			s.Git.Ref = "release/1.2"
			s.Git.ContextDir = "app"
			s.Git.Dockerfile = "docker/Dockerfile.prod"
		},
		"namespaced repo": func(s *v1alpha1.BuildConfigSpec) {
			s.Output.Repository = "team/web"
		},
		"tag template": func(s *v1alpha1.BuildConfigSpec) { s.Output.Tag = "{ref}-{sha}" },
		"build args": func(s *v1alpha1.BuildConfigSpec) {
			s.BuildArgs = map[string]string{"VERSION": "1", "_x": "y"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := validBuildConfig("web")
			mutate(s)
			if err := ValidateBuildConfig("web", s); err != nil {
				t.Fatalf("want valid, got %v", err)
			}
		})
	}
}

func TestValidateBuildConfigRejects(t *testing.T) {
	cases := []struct {
		name   string
		config string
		mutate func(*v1alpha1.BuildConfigSpec)
		want   string // a substring of the error, naming the field
	}{
		{"long name", strings.Repeat("a", 41), nil, "metadata.name"},
		{"no url", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "" }, "spec.git.url"},
		{"http", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "http://example.com/x.git" }, "spec.git.url"},
		{"file scheme", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "file:///etc/passwd" }, "spec.git.url"},
		{"option-like url", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "-oProxyCommand=x" }, "spec.git.url"},
		{"credentials in url", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "https://user:pass@example.com/x.git" }, "credentials"},
		{"ssh password in url", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "ssh://user:pass@example.com/x.git" }, "credentials"},
		{"no path", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Url = "https://example.com" }, "repository path"},
		{"bad ref", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Ref = "-x" }, "spec.git.ref"},
		{"ref with dots", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Ref = "a..b" }, "spec.git.ref"},
		{"absolute context", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.ContextDir = "/etc" }, "spec.git.contextDir"},
		{"context escapes", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.ContextDir = "app/../../x" }, "spec.git.contextDir"},
		{"dockerfile absolute", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Dockerfile = "/Dockerfile" }, "spec.git.dockerfile"},
		{"dockerfile is dir", "web", func(s *v1alpha1.BuildConfigSpec) { s.Git.Dockerfile = "docker/" }, "spec.git.dockerfile"},
		{"bad secret name", "web", func(s *v1alpha1.BuildConfigSpec) { s.CredentialsSecret = "Bad_Name" }, "spec.credentialsSecret"},
		{"uppercase repo", "web", func(s *v1alpha1.BuildConfigSpec) { s.Output.Repository = "Web" }, "spec.output.repository"},
		{"unknown placeholder", "web", func(s *v1alpha1.BuildConfigSpec) { s.Output.Tag = "{branch}" }, "spec.output.tag"},
		{"tag starts with dash", "web", func(s *v1alpha1.BuildConfigSpec) { s.Output.Tag = "-{ref}" }, "spec.output.tag"},
		{"bad build arg name", "web", func(s *v1alpha1.BuildConfigSpec) { s.BuildArgs = map[string]string{"1BAD": "x"} }, "spec.buildArgs"},
		{"multi-line build arg", "web", func(s *v1alpha1.BuildConfigSpec) { s.BuildArgs = map[string]string{"V": "a\nb"} }, "spec.buildArgs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validBuildConfig("web")
			if tc.mutate != nil {
				tc.mutate(s)
			}
			err := ValidateBuildConfig(tc.config, s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestExpandTag(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		tmpl, ref, want string
	}{
		{"", "main", "0123456"},
		{"{short-sha}", "main", "0123456"},
		{"{sha}", "main", sha},
		{"{ref}-{short-sha}", "feature/login", "feature-login-0123456"},
		{"v1", "main", "v1"},
	}
	for _, tc := range cases {
		got, err := ExpandTag(tc.tmpl, sha, tc.ref)
		if err != nil || got != tc.want {
			t.Errorf("ExpandTag(%q, %q) = %q, %v; want %q", tc.tmpl, tc.ref, got, err, tc.want)
		}
	}
	for _, bad := range []string{"{nope}", "-{ref}", "a b"} {
		if _, err := ExpandTag(bad, sha, "main"); err == nil {
			t.Errorf("ExpandTag(%q): want error", bad)
		}
	}
}

func TestRequiredCredentialType(t *testing.T) {
	if got := RequiredCredentialType("https://github.com/acme/web.git"); got != SecretTypeBasicAuth {
		t.Errorf("https: %s", got)
	}
	for _, u := range []string{"ssh://git@github.com/acme/web.git", "git@github.com:acme/web.git"} {
		if got := RequiredCredentialType(u); got != SecretTypeSSHAuth {
			t.Errorf("%s: %s", u, got)
		}
	}
}

func TestBasicAuthAndSSHAuthSecrets(t *testing.T) {
	cases := []struct {
		name string
		s    *v1alpha1.SecretSpec
		ok   bool
	}{
		{"basic ok", &v1alpha1.SecretSpec{Type: SecretTypeBasicAuth, Data: map[string]string{"username": "u", "password": "p"}}, true},
		{"basic without password", &v1alpha1.SecretSpec{Type: SecretTypeBasicAuth, Data: map[string]string{"username": "u"}}, false},
		{"ssh ok", &v1alpha1.SecretSpec{Type: SecretTypeSSHAuth, Data: map[string]string{SSHPrivateKeyKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"}}, true},
		{"ssh without key", &v1alpha1.SecretSpec{Type: SecretTypeSSHAuth, Data: map[string]string{}}, false},
		{"ssh not a key", &v1alpha1.SecretSpec{Type: SecretTypeSSHAuth, Data: map[string]string{SSHPrivateKeyKey: "hunter2"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSecret("creds", tc.s)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v, got %v", tc.ok, err)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error leaks a value: %v", err)
			}
		})
	}
}
