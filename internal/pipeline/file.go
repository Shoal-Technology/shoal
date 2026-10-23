package pipeline

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// FileName is the pipeline file read from the root of a repository.
const FileName = "shoal-ci.yml"

// Limits of one file (ADR 0022, sections 3, 5 and 7).
const (
	MaxFileBytes = 256 << 10
	MaxJobs      = 100
	MaxStages    = 10
	MaxRetry     = 2
	// DefaultExpire is an artifact's lifetime when expire_in is not set.
	DefaultExpire = 7 * 24 * time.Hour
	// MaxExpire is the longest an artifact may be kept.
	MaxExpire = 30 * 24 * time.Hour
)

var (
	dnsName     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	stageName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretKey   = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
	cacheKey    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	runnerTag   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	imageRefRe  = regexp.MustCompile(`^[^\s]+$`)
	whenValues  = []string{WhenOnSuccess, WhenAlways, WhenManual, WhenNever}
	retryValues = []string{ReasonRunnerError, ReasonNodeLost}
)

// refusedTop are top-level keys of GitLab's ci.json that v1 does not support,
// with the reason (ADR 0022, section 13).
var refusedTop = map[string]string{
	"include":       "reuse across files needs a resolver; it is planned for later (GL-17)",
	"default":       "defaults are not supported: set the key on each job",
	"image":         "set image on each job: a top-level image is not supported",
	"cache":         "set cache on each job: a top-level cache is not supported",
	"workflow":      "pipeline-wide rules are not supported in v1 (GL-09)",
	"services":      "sidecar services are not supported: one image per workload (GL-07)",
	"before_script": "use script: it is a list and covers the case",
	"after_script":  "not supported in v1 (GL-23)",
	"hooks":         "not supported in v1 (GL-23)",
	"pages":         "not supported in v1 (GL-19)",
	"release":       "not supported in v1 (GL-39)",
}

// refusedJob are job keys of GitLab's ci.json that v1 does not support.
var refusedJob = map[string]string{
	"extends":             "reuse across jobs needs a resolver; it is planned for later (GL-17)",
	"inherit":             "defaults are not supported in v1",
	"parallel":            "copies of a job are not supported in v1 (GL-16)",
	"trigger":             "child and downstream pipelines are not supported in v1 (GL-18)",
	"services":            "sidecar services are not supported: one image per workload (GL-07)",
	"before_script":       "use script: it is a list and covers the case",
	"after_script":        "not supported in v1 (GL-23)",
	"hooks":               "not supported in v1 (GL-23)",
	"only":                "deprecated upstream: use rules",
	"except":              "deprecated upstream: use rules",
	"run":                 "step-based jobs are not supported: use script",
	"dependencies":        "not supported in v1 (GL-25); needs gives the edges",
	"start_in":            "not supported in v1 (GL-24)",
	"interruptible":       "not supported in v1 (GL-22)",
	"coverage":            "not supported in v1 (GL-21)",
	"reports":             "not supported in v1 (GL-12)",
	"release":             "not supported in v1 (GL-39)",
	"pages":               "not supported in v1 (GL-19)",
	"id_tokens":           "needs workload identity (GL-32)",
	"identity":            "needs workload identity (GL-32)",
	"dast_configuration":  "scanners run as ordinary jobs (GL-20)",
	"entrypoint":          "keep the image default",
	"pull_policy":         "keep the image default",
	"resource_group":      "deploy locks come with environments in pass C (GL-35)",
	"manual_confirmation": "not supported in v1 (GL-29)",
}

var supportedJob = map[string]bool{
	"stage": true, "image": true, "script": true, "needs": true, "rules": true,
	"variables": true, "artifacts": true, "cache": true, "secrets": true,
	"timeout": true, "retry": true, "tags": true, "when": true,
	"allow_failure": true, "environment": true, "build": true, "deploy": true,
}

// Parse decodes and checks a shoal-ci.yml. Every problem found is returned,
// each prefixed with its line, so a file is fixed in one pass.
func Parse(data []byte) (*v1alpha1.PipelineSource, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("the file is %d bytes; at most %d", len(data), MaxFileBytes)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("the file is empty")
	}
	x := &expander{left: maxExpandedNodes}
	x.resolve(doc.Content[0])
	if x.over {
		return nil, errors.New("anchors and aliases expand to too many nodes")
	}
	mergeKeys(doc.Content[0])
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: the file must be a mapping of stages, variables and jobs", root.Line)
	}
	p := &parser{}
	p.top(root)
	return p.result()
}

// maxExpandedNodes bounds the nodes that aliases may copy in one file. A file
// built to expand exponentially (a "billion laughs" document) stops here.
const maxExpandedNodes = 200000

// expander replaces each alias with a copy of the node it names, so the checks
// below see the values. Anchors and aliases are plain YAML and allowed (ADR
// 0022, section 2).
type expander struct {
	left int
	over bool
}

func (x *expander) resolve(n *yaml.Node) {
	for i, c := range n.Content {
		if c.Kind == yaml.AliasNode && c.Alias != nil {
			n.Content[i] = x.copy(c.Alias)
			continue
		}
		x.resolve(c)
	}
}

func (x *expander) copy(src *yaml.Node) *yaml.Node {
	x.left--
	if x.left < 0 {
		x.over = true
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
	cp := *src
	cp.Content = make([]*yaml.Node, len(src.Content))
	for i, c := range src.Content {
		if x.over {
			break
		}
		if c.Kind == yaml.AliasNode && c.Alias != nil {
			cp.Content[i] = x.copy(c.Alias)
		} else {
			cp.Content[i] = x.copy(c)
		}
	}
	return &cp
}

// mergeKeys applies YAML merge keys ("<<: *base") to each mapping under n. A
// key the mapping sets itself wins over the merged one.
func mergeKeys(n *yaml.Node) {
	if n.Kind == yaml.MappingNode {
		explicit := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i]; k.Value != "<<" {
				explicit[k.Value] = true
			}
		}
		var out []*yaml.Node
		added := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value != "<<" {
				out = append(out, k, v)
				continue
			}
			if v.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				mk := v.Content[j]
				if explicit[mk.Value] || added[mk.Value] {
					continue
				}
				added[mk.Value] = true
				out = append(out, mk, v.Content[j+1])
			}
		}
		n.Content = out
	}
	for _, c := range n.Content {
		mergeKeys(c)
	}
}

type lineErr struct {
	line int
	msg  string
}

// parsedJob keeps what the edge checks need after every job is read.
type parsedJob struct {
	job       *v1alpha1.PipelineJob
	line      int
	needLines []int
	fromLine  int
}

type parser struct {
	errs     []lineErr
	stages   []string
	vars     map[string]string
	sawStage bool
	jobs     []parsedJob
	byName   map[string]int // job name -> index in jobs
}

func (p *parser) errAt(line int, format string, args ...any) {
	p.errs = append(p.errs, lineErr{line: line, msg: fmt.Sprintf("line %d: ", line) + fmt.Sprintf(format, args...)})
}

func (p *parser) top(root *yaml.Node) {
	type pair struct{ k, v *yaml.Node }
	var jobPairs []pair
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "stages":
			p.sawStage = true
			p.stagesOf(k, v)
		case "variables":
			p.vars = p.variablesOf(k, v, "variables")
		default:
			if why, ok := refusedTop[k.Value]; ok {
				p.errAt(k.Line, "%q is not supported in shoal-ci.yml: %s", k.Value, why)
				continue
			}
			if v.Kind != yaml.MappingNode {
				p.errAt(k.Line, "unknown key %q: top-level keys are stages, variables and job names (each job is a mapping)", k.Value)
				continue
			}
			jobPairs = append(jobPairs, pair{k, v})
		}
	}
	if !p.sawStage {
		p.errAt(root.Line, "stages is required: list the stages in the order they run")
	}
	p.byName = map[string]int{}
	for _, jp := range jobPairs {
		p.job(jp.k, jp.v)
	}
	p.checkEdges()
}

func (p *parser) stagesOf(k, v *yaml.Node) {
	if v.Kind != yaml.SequenceNode || len(v.Content) == 0 {
		p.errAt(k.Line, "stages must be a non-empty list of stage names")
		return
	}
	if len(v.Content) > MaxStages {
		p.errAt(k.Line, "stages: at most %d stages per file", MaxStages)
	}
	for _, s := range v.Content {
		switch {
		case s.Kind != yaml.ScalarNode || !stageName.MatchString(s.Value):
			p.errAt(s.Line, "stage %q: use lowercase letters, digits and -, starting with a letter", s.Value)
		case slices.Contains(p.stages, s.Value):
			p.errAt(s.Line, "stage %q is listed twice", s.Value)
		default:
			p.stages = append(p.stages, s.Value)
		}
	}
}

func (p *parser) variablesOf(k, v *yaml.Node, what string) map[string]string {
	if v.Kind != yaml.MappingNode {
		p.errAt(k.Line, "%s must be a mapping of names to values", what)
		return nil
	}
	out := map[string]string{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		kn, vn := v.Content[i], v.Content[i+1]
		switch {
		case !envName.MatchString(kn.Value):
			p.errAt(kn.Line, "%s: %q is not a valid variable name", what, kn.Value)
		case vn.Kind != yaml.ScalarNode:
			p.errAt(kn.Line, "%s: the value of %s must be a string", what, kn.Value)
		default:
			out[kn.Value] = vn.Value
		}
	}
	return out
}

func (p *parser) job(k, v *yaml.Node) {
	name := k.Value
	if !dnsName.MatchString(name) || len(name) > 40 {
		p.errAt(k.Line, "job %q: use lowercase letters, digits and -, at most 40 characters", name)
		return
	}
	if _, dup := p.byName[name]; dup {
		p.errAt(k.Line, "job %q is defined twice", name)
		return
	}
	where := fmt.Sprintf("job %q", name)
	job := &v1alpha1.PipelineJob{Name: name}
	pj := parsedJob{job: job, line: k.Line}
	for i := 0; i+1 < len(v.Content); i += 2 {
		kn, vn := v.Content[i], v.Content[i+1]
		key := kn.Value
		if why, ok := refusedJob[key]; ok {
			p.errAt(kn.Line, "%s: %q is not supported in shoal-ci.yml: %s", where, key, why)
			continue
		}
		if !supportedJob[key] {
			p.errAt(kn.Line, "%s: unknown key %q", where, key)
			continue
		}
		p.jobKey(where, &pj, kn, vn)
	}
	p.jobChecks(where, &pj)
	p.byName[name] = len(p.jobs)
	p.jobs = append(p.jobs, pj)
}

// jobKey reads one supported key of a job.
func (p *parser) jobKey(where string, pj *parsedJob, kn, vn *yaml.Node) {
	job := pj.job
	switch kn.Value {
	case "stage":
		switch {
		case vn.Kind != yaml.ScalarNode:
			p.errAt(vn.Line, "%s: stage must be a stage name", where)
		case !p.sawStage:
		case !slices.Contains(p.stages, vn.Value):
			p.errAt(vn.Line, "%s: stage %q is not in stages (%s)", where, vn.Value, strings.Join(p.stages, ", "))
		default:
			job.Stage = vn.Value
		}
	case "image":
		if vn.Kind != yaml.ScalarNode || !imageRefRe.MatchString(vn.Value) {
			p.errAt(vn.Line, "%s: image must be an image reference without spaces", where)
			return
		}
		job.Image = vn.Value
	case "script":
		job.Script = p.stringList(where, "script", kn, vn, false)
	case "needs":
		for _, n := range p.stringList(where, "needs", kn, vn, false) {
			job.Needs = append(job.Needs, n)
			pj.needLines = append(pj.needLines, vn.Line)
		}
	case "rules":
		p.rulesOf(where, job, kn, vn)
	case "variables":
		job.Variables = p.variablesOf(kn, vn, where+" variables")
	case "artifacts":
		p.artifactsOf(where, job, kn, vn)
	case "cache":
		p.cacheOf(where, job, kn, vn)
	case "secrets":
		p.secretsOf(where, job, kn, vn)
	case "timeout":
		d, err := parseDuration(vn.Value)
		if vn.Kind != yaml.ScalarNode || err != nil || d <= 0 {
			p.errAt(vn.Line, "%s: timeout must be a positive duration such as 15m", where)
			return
		}
		job.Timeout = durationpb.New(d)
	case "retry":
		p.retryOf(where, job, kn, vn)
	case "tags":
		for _, t := range p.stringList(where, "tags", kn, vn, false) {
			if !runnerTag.MatchString(t) {
				p.errAt(vn.Line, "%s: tag %q: use lowercase letters, digits, . _ and -", where, t)
				continue
			}
			job.Tags = append(job.Tags, t)
		}
	case "when":
		if vn.Kind != yaml.ScalarNode || !slices.Contains(whenValues, vn.Value) {
			p.errAt(vn.Line, "%s: when must be one of %s", where, strings.Join(whenValues, ", "))
			return
		}
		job.When = vn.Value
	case "allow_failure":
		if vn.Kind != yaml.ScalarNode || (vn.Value != "true" && vn.Value != "false") {
			p.errAt(vn.Line, "%s: allow_failure must be true or false", where)
			return
		}
		job.AllowFailure = vn.Value == "true"
	case "environment":
		p.environmentOf(where, job, kn, vn)
	case "build":
		if vn.Kind != yaml.ScalarNode || !dnsName.MatchString(vn.Value) {
			p.errAt(vn.Line, "%s: build names a BuildConfig: use lowercase letters, digits and -", where)
			return
		}
		job.Build = vn.Value
	case "deploy":
		p.deployOf(where, pj, kn, vn)
	}
}

// stringList reads a list of non-empty strings.
func (p *parser) stringList(where, key string, kn, vn *yaml.Node, allowEmpty bool) []string {
	if vn.Kind != yaml.SequenceNode {
		p.errAt(kn.Line, "%s: %s must be a list", where, key)
		return nil
	}
	if len(vn.Content) == 0 && !allowEmpty {
		p.errAt(kn.Line, "%s: %s must not be empty", where, key)
		return nil
	}
	var out []string
	for _, item := range vn.Content {
		if item.Kind != yaml.ScalarNode || item.Value == "" {
			p.errAt(item.Line, "%s: each entry of %s must be a non-empty string", where, key)
			continue
		}
		out = append(out, item.Value)
	}
	return out
}

func (p *parser) rulesOf(where string, job *v1alpha1.PipelineJob, kn, vn *yaml.Node) {
	if vn.Kind != yaml.SequenceNode || len(vn.Content) == 0 {
		p.errAt(kn.Line, "%s: rules must be a non-empty list", where)
		return
	}
	for _, rn := range vn.Content {
		if rn.Kind != yaml.MappingNode {
			p.errAt(rn.Line, "%s: each rule is a mapping with if, changes or when", where)
			continue
		}
		var rule v1alpha1.PipelineRule
		hasCond := false
		for i := 0; i+1 < len(rn.Content); i += 2 {
			k, v := rn.Content[i], rn.Content[i+1]
			switch k.Value {
			case "if":
				if v.Kind != yaml.ScalarNode {
					p.errAt(v.Line, "%s: if must be an expression", where)
					continue
				}
				if _, err := ParseCondition(v.Value); err != nil {
					p.errAt(v.Line, "%s: %v", where, err)
					continue
				}
				rule.Condition = strings.TrimSpace(v.Value)
				hasCond = true
			case "changes":
				for _, pat := range p.stringList(where, "changes", k, v, false) {
					if err := ValidatePattern(pat); err != nil {
						p.errAt(v.Line, "%s: changes: %v", where, err)
						continue
					}
					rule.Changes = append(rule.Changes, pat)
				}
				hasCond = hasCond || len(rule.Changes) > 0
			case "when":
				if v.Kind != yaml.ScalarNode || !slices.Contains(whenValues, v.Value) {
					p.errAt(v.Line, "%s: rule when must be one of %s", where, strings.Join(whenValues, ", "))
					continue
				}
				rule.When = v.Value
			default:
				p.errAt(k.Line, "%s: rule key %q is not supported (use if, changes or when)", where, k.Value)
			}
		}
		if !hasCond {
			p.errAt(rn.Line, "%s: a rule needs if or changes", where)
			continue
		}
		job.Rules = append(job.Rules, &rule)
	}
}

func (p *parser) artifactsOf(where string, job *v1alpha1.PipelineJob, kn, vn *yaml.Node) {
	if vn.Kind != yaml.MappingNode {
		p.errAt(kn.Line, "%s: artifacts must be a mapping with paths and expire_in", where)
		return
	}
	art := &v1alpha1.JobArtifacts{}
	for i := 0; i+1 < len(vn.Content); i += 2 {
		k, v := vn.Content[i], vn.Content[i+1]
		switch k.Value {
		case "paths":
			for _, path := range p.stringList(where, "artifacts.paths", k, v, false) {
				if err := checkRelative(path); err != nil {
					p.errAt(v.Line, "%s: artifacts.paths: %v", where, err)
					continue
				}
				art.Paths = append(art.Paths, path)
			}
		case "expire_in":
			d, err := parseDuration(v.Value)
			if v.Kind != yaml.ScalarNode || err != nil || d <= 0 || d > MaxExpire {
				p.errAt(v.Line, "%s: artifacts.expire_in must be a duration from 1h to 30d, such as 7d", where)
				continue
			}
			art.ExpireIn = durationpb.New(d)
		default:
			p.errAt(k.Line, "%s: artifacts key %q is not supported (use paths or expire_in)", where, k.Value)
		}
	}
	if len(art.Paths) == 0 {
		p.errAt(kn.Line, "%s: artifacts.paths is required", where)
		return
	}
	if art.ExpireIn == nil {
		art.ExpireIn = durationpb.New(DefaultExpire)
	}
	job.Artifacts = art
}

func (p *parser) cacheOf(where string, job *v1alpha1.PipelineJob, kn, vn *yaml.Node) {
	if vn.Kind != yaml.MappingNode {
		p.errAt(kn.Line, "%s: cache must be a mapping with key and paths", where)
		return
	}
	c := &v1alpha1.JobCache{}
	for i := 0; i+1 < len(vn.Content); i += 2 {
		k, v := vn.Content[i], vn.Content[i+1]
		switch k.Value {
		case "key":
			switch {
			case strings.ContainsAny(v.Value, "{$"):
				p.errAt(v.Line, "%s: templated cache keys are not supported yet: use a literal key", where)
			case v.Kind != yaml.ScalarNode || !cacheKey.MatchString(v.Value):
				p.errAt(v.Line, "%s: cache key must be 1 to 64 letters, digits, . _ or -", where)
			default:
				c.Key = v.Value
			}
		case "paths":
			for _, path := range p.stringList(where, "cache.paths", k, v, false) {
				if err := checkRelative(path); err != nil {
					p.errAt(v.Line, "%s: cache.paths: %v", where, err)
					continue
				}
				c.Paths = append(c.Paths, path)
			}
		default:
			p.errAt(k.Line, "%s: cache key %q is not supported (use key or paths)", where, k.Value)
		}
	}
	if c.Key == "" {
		p.errAt(kn.Line, "%s: cache.key is required", where)
		return
	}
	job.Cache = c
}

func (p *parser) secretsOf(where string, job *v1alpha1.PipelineJob, kn, vn *yaml.Node) {
	if vn.Kind != yaml.MappingNode {
		p.errAt(kn.Line, "%s: secrets must map variable names to {secret, key}", where)
		return
	}
	out := map[string]*v1alpha1.SecretRef{}
	for i := 0; i+1 < len(vn.Content); i += 2 {
		k, v := vn.Content[i], vn.Content[i+1]
		if !envName.MatchString(k.Value) {
			p.errAt(k.Line, "%s: %q is not a valid variable name", where, k.Value)
			continue
		}
		if v.Kind != yaml.MappingNode {
			p.errAt(v.Line, "%s: secrets.%s must be {secret: NAME, key: KEY}: external vaults are not supported", where, k.Value)
			continue
		}
		ref := &v1alpha1.SecretRef{}
		for j := 0; j+1 < len(v.Content); j += 2 {
			sk, sv := v.Content[j], v.Content[j+1]
			switch sk.Value {
			case "secret":
				if sv.Kind != yaml.ScalarNode || !dnsName.MatchString(sv.Value) {
					p.errAt(sv.Line, "%s: secrets.%s.secret names a Secret: use lowercase letters, digits and -", where, k.Value)
					continue
				}
				ref.Secret = sv.Value
			case "key":
				if sv.Kind != yaml.ScalarNode || !secretKey.MatchString(sv.Value) {
					p.errAt(sv.Line, "%s: secrets.%s.key is not a valid key", where, k.Value)
					continue
				}
				ref.Key = sv.Value
			default:
				p.errAt(sk.Line, "%s: secrets.%s key %q is not supported (use secret and key)", where, k.Value, sk.Value)
			}
		}
		if ref.Secret == "" || ref.Key == "" {
			p.errAt(v.Line, "%s: secrets.%s needs both secret and key", where, k.Value)
			continue
		}
		out[k.Value] = ref
	}
	if len(out) > 0 {
		job.Secrets = out
	}
}

func (p *parser) retryOf(where string, job *v1alpha1.PipelineJob, kn, vn *yaml.Node) {
	if vn.Kind != yaml.MappingNode {
		p.errAt(kn.Line, "%s: retry must be a mapping with max and when", where)
		return
	}
	r := &v1alpha1.JobRetry{}
	for i := 0; i+1 < len(vn.Content); i += 2 {
		k, v := vn.Content[i], vn.Content[i+1]
		switch k.Value {
		case "max":
			n, err := strconv.Atoi(v.Value)
			if v.Kind != yaml.ScalarNode || err != nil || n < 0 || n > MaxRetry {
				p.errAt(v.Line, "%s: retry.max must be from 0 to %d", where, MaxRetry)
				continue
			}
			r.Max = int32(n)
		case "when":
			for _, w := range p.stringList(where, "retry.when", k, v, false) {
				switch {
				case w == "script" || w == "script_failure" || w == "failure" || w == "exit_code":
					p.errAt(v.Line, "%s: retry.when %q is not retried: a script that exits non-zero is never retried", where, w)
				case !slices.Contains(retryValues, w):
					p.errAt(v.Line, "%s: retry.when %q is not supported (use runner_error or node_lost)", where, w)
				default:
					r.When = append(r.When, w)
				}
			}
		default:
			p.errAt(k.Line, "%s: retry key %q is not supported (use max or when)", where, k.Value)
		}
	}
	if len(r.When) == 0 {
		r.When = slices.Clone(retryValues)
	}
	job.Retry = r
}

func (p *parser) environmentOf(where string, job *v1alpha1.PipelineJob, kn, vn *yaml.Node) {
	if vn.Kind != yaml.MappingNode {
		p.errAt(kn.Line, "%s: environment must be a mapping with name", where)
		return
	}
	env := &v1alpha1.JobEnvironment{}
	for i := 0; i+1 < len(vn.Content); i += 2 {
		k, v := vn.Content[i], vn.Content[i+1]
		switch k.Value {
		case "name":
			if v.Kind != yaml.ScalarNode || !dnsName.MatchString(v.Value) {
				p.errAt(v.Line, "%s: environment.name must be lowercase letters, digits and -", where)
				continue
			}
			env.Name = v.Value
		case "approval":
			if v.Kind != yaml.ScalarNode || v.Value != "required" {
				p.errAt(v.Line, "%s: environment.approval must be required", where)
				continue
			}
			env.Approval = v.Value
		default:
			p.errAt(k.Line, "%s: environment key %q is not supported (use name and approval)", where, k.Value)
		}
	}
	if env.Name == "" {
		p.errAt(kn.Line, "%s: environment.name is required", where)
		return
	}
	job.Environment = env
}

func (p *parser) deployOf(where string, pj *parsedJob, kn, vn *yaml.Node) {
	if vn.Kind != yaml.MappingNode {
		p.errAt(kn.Line, "%s: deploy must be a mapping with workload and from", where)
		return
	}
	d := &v1alpha1.JobDeploy{}
	for i := 0; i+1 < len(vn.Content); i += 2 {
		k, v := vn.Content[i], vn.Content[i+1]
		switch k.Value {
		case "workload":
			if v.Kind != yaml.ScalarNode || !dnsName.MatchString(v.Value) {
				p.errAt(v.Line, "%s: deploy.workload names a Workload: use lowercase letters, digits and -", where)
				continue
			}
			d.Workload = v.Value
		case "from":
			if v.Kind != yaml.ScalarNode {
				p.errAt(v.Line, "%s: deploy.from names a build job", where)
				continue
			}
			d.From = v.Value
			pj.fromLine = v.Line
		default:
			p.errAt(k.Line, "%s: deploy key %q is not supported (use workload and from)", where, k.Value)
		}
	}
	if d.Workload == "" || d.From == "" {
		p.errAt(kn.Line, "%s: deploy needs workload and from", where)
		return
	}
	pj.job.Deploy = d
}

// jobChecks checks what needs the whole job: which kind it is.
func (p *parser) jobChecks(where string, pj *parsedJob) {
	job := pj.job
	if job.Stage == "" && p.sawStage {
		p.errAt(pj.line, "%s: stage is required", where)
	}
	isBuild, isDeploy := job.Build != "", job.Deploy != nil
	hasScript := job.Image != "" || len(job.Script) > 0
	switch {
	case isBuild && isDeploy:
		p.errAt(pj.line, "%s: a job is a build or a deploy, not both", where)
	case (isBuild || isDeploy) && hasScript:
		p.errAt(pj.line, "%s: a build or deploy job cannot also have image or script", where)
	case !isBuild && !isDeploy && job.Image == "":
		p.errAt(pj.line, "%s: image is required for a script job", where)
	case !isBuild && !isDeploy && len(job.Script) == 0:
		p.errAt(pj.line, "%s: script is required for a script job", where)
	}
}

// checkEdges checks needs and deploy.from across jobs, and the job count.
func (p *parser) checkEdges() {
	if len(p.jobs) == 0 {
		p.errAt(1, "the file has no jobs: add at least one job with a stage")
	}
	if len(p.jobs) > MaxJobs {
		p.errAt(1, "%d jobs: at most %d per file", len(p.jobs), MaxJobs)
	}
	stageOf := func(name string) (int, bool) {
		i, ok := p.byName[name]
		if !ok {
			return 0, false
		}
		return slices.Index(p.stages, p.jobs[i].job.Stage), true
	}
	for _, pj := range p.jobs {
		job := pj.job
		for i, need := range job.Needs {
			line := pj.needLines[i]
			if need == job.Name {
				p.errAt(line, "job %q: needs itself", job.Name)
				continue
			}
			ns, ok := stageOf(need)
			if !ok {
				p.errAt(line, "job %q: needs %q, which is not a job in this file", job.Name, need)
				continue
			}
			if js := slices.Index(p.stages, job.Stage); js >= 0 && ns >= js {
				p.errAt(line, "job %q: needs %q, which is in stage %q, not an earlier stage than %q (needs edges go to earlier stages)", job.Name, need, p.jobs[p.byName[need]].job.Stage, job.Stage)
			}
		}
		if d := job.Deploy; d != nil {
			ref, ok := p.byName[d.From]
			switch {
			case !ok:
				p.errAt(pj.fromLine, "job %q: deploy.from %q is not a job in this file", job.Name, d.From)
			case p.jobs[ref].job.Build == "":
				p.errAt(pj.fromLine, "job %q: deploy.from %q must be a build job (one with build:)", job.Name, d.From)
			case !slices.Contains(job.Needs, d.From):
				p.errAt(pj.fromLine, "job %q: deploy.from %q must also be listed in needs", job.Name, d.From)
			}
		}
	}
}

func (p *parser) result() (*v1alpha1.PipelineSource, error) {
	if len(p.errs) > 0 {
		sort.SliceStable(p.errs, func(i, j int) bool { return p.errs[i].line < p.errs[j].line })
		msgs := make([]error, 0, len(p.errs))
		for _, e := range p.errs {
			msgs = append(msgs, errors.New(e.msg))
		}
		return nil, errors.Join(msgs...)
	}
	src := &v1alpha1.PipelineSource{Stages: p.stages, Variables: p.vars}
	for _, pj := range p.jobs {
		src.Jobs = append(src.Jobs, pj.job)
	}
	return src, nil
}

// checkRelative checks a path inside the repository or the workspace.
func checkRelative(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("a path is empty")
	case strings.HasPrefix(path, "/"), strings.Contains(path, "\\"):
		return fmt.Errorf("%q must be a relative path", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return fmt.Errorf("%q must not leave the workspace", path)
		}
	}
	return nil
}

// parseDuration reads a Go duration, and also whole days as "7d".
func parseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
