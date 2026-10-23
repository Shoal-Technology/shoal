package resource

// RunnerLabel makes a node a runner: it runs pipeline jobs (ADR 0022, pass B).
// The label's value must be "true". A node without it never runs a job.
const RunnerLabel = "shoal.dev/runner"

// IsRunner reports whether a node's labels make it a runner.
func IsRunner(labels map[string]string) bool { return labels[RunnerLabel] == "true" }

// NodeMatchesTags reports whether a node may run a job with these tags. Every
// tag must be a label of the node whose value is "true" (ADR 0022, section 5).
// A job without tags runs on any runner.
func NodeMatchesTags(labels map[string]string, tags []string) bool {
	for _, t := range tags {
		if labels[t] != "true" {
			return false
		}
	}
	return true
}

// BuildTriggerPipeline is the trigger of a Build started by a pipeline's build
// step (ADR 0022, section 4).
const BuildTriggerPipeline = "pipeline"

// TriggerPush is the trigger of a PipelineRun started by a push (ADR 0022,
// section 4).
const TriggerPush = "push"

// DefaultProtectedBranches are the branches whose pushes start protected runs
// (ADR 0022, section 7). The server config does not change them in pass B.
var DefaultProtectedBranches = []string{"main"}

// TrustOfPush is the trust of a run started by a push to branch: protected for
// a protected branch, same-repo otherwise. A same-repo run gets no secrets.
func TrustOfPush(branch string, protected []string) string {
	for _, p := range protected {
		if p == branch {
			return TrustProtected
		}
	}
	return TrustSameRepo
}

// TrustSameRepo is the trust of a run started by a push to an unprotected
// branch, or by a pull request from the same repository (ADR 0022, section 7).
const TrustSameRepo = "same-repo"

// Job container limits, applied when the file asks for none (ADR 0022,
// section 5). The server config may change them in a later pass.
const (
	DefaultJobCPUMillis   = 1000
	DefaultJobMemoryBytes = 1 << 30 // 1 GiB
	DefaultJobPids        = 512
	// DefaultJobTimeout bounds a job whose file gives no timeout.
	DefaultJobTimeoutSeconds = 3600
	// MaxJobTimeoutSeconds is the server-side ceiling (ADR 0022, section 7).
	MaxJobTimeoutSeconds = 7200
)
