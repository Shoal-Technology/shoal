package resource

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Environments, approvals and deploys (ADR 0022, pass C, section 10).
const (
	// EnvironmentMaxNameLen matches PipelineMaxNameLen, so a deploy job's name
	// stays readable.
	EnvironmentMaxNameLen = 40
	// MaxRequiredApprovals bounds the approvals an environment may ask for.
	MaxRequiredApprovals = 10
	// ApprovalTimeout is how long a gated job waits for approval before it
	// ends Failed. Not in the ADR; chosen for pass C and recorded there.
	ApprovalTimeout = 24 * time.Hour
	// EnvironmentHistory is how many deploys an environment keeps.
	EnvironmentHistory = 10
	// ApprovalRequired is the only approval value the file accepts.
	ApprovalRequired = "required"
	// DecisionApproved and DecisionRejected are the decisions on a gated job.
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
	// ActionDeploy is the one action a job token authorises.
	ActionDeploy = "deploy"
)

// DefaultEnvironment fills in the defaults of an Environment spec: a protected
// environment needs one approval unless it names more.
func DefaultEnvironment(s *v1alpha1.EnvironmentSpec) {
	if s.GetProtected() && s.GetRequiredApprovals() == 0 {
		s.RequiredApprovals = 1
	}
}

// ValidateEnvironment checks a defaulted Environment spec and reports every
// problem, each prefixed with its field path.
func ValidateEnvironment(name string, s *v1alpha1.EnvironmentSpec) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if !ValidName(name, EnvironmentMaxNameLen) {
		bad("metadata.name", "must be a lowercase DNS label of at most %d characters", EnvironmentMaxNameLen)
	}
	if u := s.GetUrl(); u != "" {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			bad("spec.url", "%q is not an http or https URL", u)
		}
	}
	if n := s.GetRequiredApprovals(); n < 0 || n > MaxRequiredApprovals {
		bad("spec.requiredApprovals", "must be from 0 to %d", MaxRequiredApprovals)
	}
	for _, w := range s.GetWorkloads() {
		if !ValidName(w, 63) {
			bad("spec.workloads", "%q is not a Workload name", w)
		}
	}
	return errors.Join(errs...)
}

// JobGated reports whether a job waits at an approval gate before it starts:
// its file asks for approval, or its environment is protected. A job with no
// environment is never gated.
func JobGated(job *v1alpha1.PipelineJob, env *v1alpha1.EnvironmentSpec) bool {
	if job.GetEnvironment() == nil {
		return false
	}
	return job.GetEnvironment().GetApproval() == ApprovalRequired || env.GetProtected()
}

// ApprovalsNeeded is how many approvals a gated job needs. A job that is not
// gated needs none.
func ApprovalsNeeded(gated bool, env *v1alpha1.EnvironmentSpec) int {
	if !gated {
		return 0
	}
	return max(int(env.GetRequiredApprovals()), 1)
}

// ApprovedBy returns the approvers whose decision is approved, in the order
// they were recorded, each once.
func ApprovedBy(approvals []*v1alpha1.JobApproval) []string {
	var out []string
	for _, a := range approvals {
		if a.GetDecision() == DecisionApproved && !slices.Contains(out, a.GetBy()) {
			out = append(out, a.GetBy())
		}
	}
	return out
}

// Rejection returns the first rejection among approvals, or nil.
func Rejection(approvals []*v1alpha1.JobApproval) *v1alpha1.JobApproval {
	for _, a := range approvals {
		if a.GetDecision() == DecisionRejected {
			return a
		}
	}
	return nil
}

// BuildOutputKey is the run variable that carries a build step's image or
// digest to later jobs (ADR 0022, section 4). job is the build job's name.
func BuildOutputKey(job, field string) string {
	return "SHOAL_BUILD_" + strings.ToUpper(strings.ReplaceAll(job, "-", "_")) + "_" + field
}

// ProtectedBranchesOf returns the branches whose pushes start protected runs of
// a Pipeline: the list the Pipeline names, or main when it names none.
func ProtectedBranchesOf(s *v1alpha1.PipelineSpec) []string {
	if len(s.GetProtectedBranches()) == 0 {
		return DefaultProtectedBranches
	}
	return s.GetProtectedBranches()
}
