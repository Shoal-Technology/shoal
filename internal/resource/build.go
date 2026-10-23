package resource

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Builds (ADR 0021). A BuildConfig is a recipe; a Build is one run of it.
const (
	// BuildConfigMaxNameLen keeps "<config>-<suffix>" a valid name for a Build.
	BuildConfigMaxNameLen = 40
	// BuildSuffixLen is the length of the random suffix of a Build's name.
	BuildSuffixLen = 5
	// MaxBuildHistory is how many finished Builds each BuildConfig keeps.
	MaxBuildHistory = 10

	// BuildTriggerManual is the trigger of a Build started with StartBuild.
	// BuildTriggerWebhook (webhook.go) is the trigger of one started by a push.
	BuildTriggerManual = "manual"

	DefaultGitRef      = "HEAD"
	DefaultContextDir  = "."
	DefaultDockerfile  = "Dockerfile"
	DefaultTagTemplate = "{short-sha}"

	maxGitURL        = 2048
	maxBuildArgs     = 64
	maxBuildArgBytes = 16 << 10
)

// Build phases (ADR 0021). The names follow OpenShift's BuildPhase (OB-02).
// Error is an infrastructure failure (no executor, the job could not start or
// was lost); Failed is a build that ran and failed.
const (
	BuildNew       = "New"
	BuildPending   = "Pending"
	BuildRunning   = "Running"
	BuildComplete  = "Complete"
	BuildFailed    = "Failed"
	BuildError     = "Error"
	BuildCancelled = "Cancelled"
)

// BuildTerminal reports whether a Build phase is final.
func BuildTerminal(phase string) bool {
	switch phase {
	case BuildComplete, BuildFailed, BuildError, BuildCancelled:
		return true
	}
	return false
}

var (
	// ociRepository is the repository grammar of the OCI distribution spec.
	ociRepository = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	buildArgName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// imageTag is the tag grammar of the OCI distribution spec.
	imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	gitRef   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
	// scpURL is the scp-like form of an ssh remote: git@host:path.
	scpURL = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9_~][^\s]*$`)
)

// sample values used to check a tag template when a config is applied.
const (
	sampleSHA = "0123456789abcdef0123456789abcdef01234567"
	sampleRef = "main"
)

// BuildName returns the name of a Build of config, given its random suffix.
func BuildName(config, suffix string) string { return config + "-" + suffix }

// DefaultBuildConfig fills in the defaults of a BuildConfig spec. name is the
// object's name; it is the default repository.
func DefaultBuildConfig(name string, s *v1alpha1.BuildConfigSpec) {
	if s.Git == nil {
		s.Git = &v1alpha1.GitSource{}
	}
	if s.Git.Ref == "" {
		s.Git.Ref = DefaultGitRef
	}
	if s.Git.ContextDir == "" {
		s.Git.ContextDir = DefaultContextDir
	}
	if s.Git.Dockerfile == "" {
		s.Git.Dockerfile = DefaultDockerfile
	}
	if s.Output == nil {
		s.Output = &v1alpha1.BuildOutput{}
	}
	if s.Output.Repository == "" {
		s.Output.Repository = name
	}
	if s.Output.Tag == "" {
		s.Output.Tag = DefaultTagTemplate
	}
}

// ValidateBuildConfig checks a defaulted BuildConfig spec and reports every
// problem found, each prefixed with its field path. Credentials are not
// checked here: the Secret may be applied later, so StartBuild checks it.
func ValidateBuildConfig(name string, s *v1alpha1.BuildConfigSpec) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if !ValidName(name, BuildConfigMaxNameLen) {
		bad("metadata.name", "must be a lowercase DNS label of at most %d characters", BuildConfigMaxNameLen)
	}
	g := s.GetGit()
	if err := checkGitURL(g.GetUrl()); err != nil {
		bad("spec.git.url", "%v", err)
	}
	if r := g.GetRef(); !validGitRef(r) {
		bad("spec.git.ref", "%q is not a branch, tag or commit name", r)
	}
	if err := checkRelPath(g.GetContextDir()); err != nil {
		bad("spec.git.contextDir", "%v", err)
	}
	if err := checkRelPath(g.GetDockerfile()); err != nil {
		bad("spec.git.dockerfile", "%v", err)
	} else if d := g.GetDockerfile(); d == "." || strings.HasSuffix(d, "/") {
		bad("spec.git.dockerfile", "must name a file")
	}
	if c := s.GetCredentialsSecret(); c != "" && !ValidName(c, 63) {
		bad("spec.credentialsSecret", "invalid Secret name %q", c)
	}
	if r := s.GetOutput().GetRepository(); len(r) > 255 || !ociRepository.MatchString(r) {
		bad("spec.output.repository", "%q is not a valid repository name (lowercase letters and digits, with . _ - separators and / for namespaces)", r)
	}
	if _, err := ExpandTag(s.GetOutput().GetTag(), sampleSHA, sampleRef); err != nil {
		bad("spec.output.tag", "%v", err)
	}
	args := s.GetBuildArgs()
	if len(args) > maxBuildArgs {
		bad("spec.buildArgs", "at most %d arguments", maxBuildArgs)
	}
	size := 0
	for _, k := range slices.Sorted(maps.Keys(args)) {
		v := args[k]
		size += len(k) + len(v)
		if !buildArgName.MatchString(k) {
			bad("spec.buildArgs", "invalid name %q (letters, digits and _, not starting with a digit)", k)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			bad("spec.buildArgs", "the value of %s must be one line", k)
		}
	}
	if size > maxBuildArgBytes {
		bad("spec.buildArgs", "%d bytes, at most %d", size, maxBuildArgBytes)
	}
	if w := s.GetWebhook(); w != nil {
		errs = append(errs, validateWebhook(w, g.GetRef()))
	}
	if u := s.GetOnSuccess().GetUpdateWorkload(); u != "" && !ValidName(u, 63) {
		bad("spec.onSuccess.updateWorkload", "invalid Workload name %q", u)
	}
	return errors.Join(errs...)
}

// checkGitURL accepts https://host/path, ssh://host/path and git@host:path.
// Other schemes are refused, and so are URLs that start with '-' (they would be
// read as options by git). Credentials stay out of the URL.
func checkGitURL(u string) error {
	switch {
	case u == "":
		return errors.New("required: https://host/path, ssh://host/path or git@host:path")
	case len(u) > maxGitURL:
		return fmt.Errorf("longer than %d characters", maxGitURL)
	case strings.HasPrefix(u, "-") || strings.ContainsAny(u, " \t\r\n\x00\\"):
		return errors.New("must not start with '-' or contain spaces, backslashes or control characters")
	case strings.HasPrefix(u, "https://"):
		return checkURLAuthority(strings.TrimPrefix(u, "https://"), false)
	case strings.HasPrefix(u, "ssh://"):
		return checkURLAuthority(strings.TrimPrefix(u, "ssh://"), true)
	case scpURL.MatchString(u):
		return nil
	}
	return errors.New("must be https://host/path, ssh://host/path or git@host:path")
}

// checkURLAuthority checks the host and path after the scheme. Only an ssh URL
// may name a user, and never with a password.
func checkURLAuthority(rest string, ssh bool) error {
	authority, path, found := strings.Cut(rest, "/")
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		if user := authority[:i]; !ssh || strings.Contains(user, ":") {
			return errors.New("credentials do not belong in the URL: use credentialsSecret")
		}
		authority = authority[i+1:]
	}
	switch {
	case authority == "":
		return errors.New("the host is missing")
	case !found || path == "":
		return errors.New("the repository path is missing")
	}
	return nil
}

// validGitRef accepts a branch, tag or commit name. HEAD is the default
// branch. Names with ".." or "//" or a trailing slash are refused.
func validGitRef(r string) bool {
	return gitRef.MatchString(r) && !strings.Contains(r, "..") && !strings.Contains(r, "//") && !strings.HasSuffix(r, "/")
}

// checkRelPath checks a path inside the repository: relative, and not leaving
// it.
func checkRelPath(p string) error {
	switch {
	case p == "":
		return errors.New("required")
	case strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\n\r"):
		return fmt.Errorf("%q must be a relative path inside the repository", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%q must not leave the repository", p)
		}
	}
	return nil
}

// RequiredCredentialType is the Secret type a build of gitURL needs:
// kubernetes.io/ssh-auth for an ssh URL, kubernetes.io/basic-auth otherwise.
func RequiredCredentialType(gitURL string) string {
	if strings.HasPrefix(gitURL, "ssh://") || scpURL.MatchString(gitURL) {
		return SecretTypeSSHAuth
	}
	return SecretTypeBasicAuth
}

// ExpandTag returns the image tag that a template gives for a commit and a
// ref. Placeholders: {sha} is the full commit, {short-sha} its first seven
// characters, and {ref} the ref with characters outside [A-Za-z0-9._-] turned
// into '-'.
func ExpandTag(tmpl, sha, ref string) (string, error) {
	if tmpl == "" {
		tmpl = DefaultTagTemplate
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	out := strings.NewReplacer("{short-sha}", short, "{sha}", sha, "{ref}", tagRef(ref)).Replace(tmpl)
	if strings.ContainsAny(out, "{}") {
		return "", fmt.Errorf("tag %q: unknown placeholder (use {sha}, {short-sha} or {ref})", tmpl)
	}
	if !imageTag.MatchString(out) {
		return "", fmt.Errorf("tag %q: not a valid image tag", out)
	}
	return out, nil
}

func tagRef(ref string) string {
	b := []byte(ref)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	if len(b) > 64 {
		b = b[:64]
	}
	return string(b)
}
