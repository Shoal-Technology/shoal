package pipeline

import (
	"fmt"
	"strings"
	"testing"
)

// adrExample is the file of ADR 0022, section 2.
const adrExample = `# shoal-ci.yml
stages: [build, test, deploy]

variables:
  NODE_ENV: ci

test-web:
  stage: test
  image: docker.io/library/node:22-alpine
  script:
    - npm ci
    - npm test
  needs: [build-web-image]
  cache:
    key: npm-v1
    paths: [node_modules/]
  secrets:
    NPM_TOKEN: { secret: npm-token, key: token }
  timeout: 15m
  retry: { max: 2, when: [runner_error] }
  tags: [linux-amd64]
  rules:
    - if: branch == "main"
    - if: event == "pull_request"
    - changes: ["web/**"]

build-web-image:
  stage: build
  build: web
  rules:
    - if: branch == "main"
    - if: tag == true

deploy-production:
  stage: deploy
  needs: [build-web-image, test-web]
  environment:
    name: production
    approval: required
  deploy:
    workload: web
    from: build-web-image
  rules:
    - if: branch == "main"
      when: manual
  timeout: 10m
`

func TestParseADRExample(t *testing.T) {
	src, err := Parse([]byte(adrExample))
	if err != nil {
		t.Fatalf("the ADR example must parse: %v", err)
	}
	if got := strings.Join(src.GetStages(), ","); got != "build,test,deploy" {
		t.Errorf("stages = %s", got)
	}
	if src.GetVariables()["NODE_ENV"] != "ci" {
		t.Errorf("variables = %v", src.GetVariables())
	}
	var names []string
	for _, j := range src.GetJobs() {
		names = append(names, j.GetName())
	}
	if got := strings.Join(names, ","); got != "test-web,build-web-image,deploy-production" {
		t.Fatalf("jobs = %s", got)
	}
	tw := src.GetJobs()[0]
	if tw.GetSecrets()["NPM_TOKEN"].GetSecret() != "npm-token" || tw.GetSecrets()["NPM_TOKEN"].GetKey() != "token" {
		t.Errorf("secrets = %v", tw.GetSecrets())
	}
	if tw.GetRetry().GetMax() != 2 || len(tw.GetRetry().GetWhen()) != 1 {
		t.Errorf("retry = %v", tw.GetRetry())
	}
	if tw.GetCache().GetKey() != "npm-v1" {
		t.Errorf("cache = %v", tw.GetCache())
	}
	if len(tw.GetRules()) != 3 || tw.GetRules()[2].GetChanges()[0] != "web/**" {
		t.Errorf("rules = %v", tw.GetRules())
	}
	if tw.GetTimeout().AsDuration().String() != "15m0s" {
		t.Errorf("timeout = %v", tw.GetTimeout())
	}
	if b := src.GetJobs()[1]; b.GetBuild() != "web" || b.GetImage() != "" {
		t.Errorf("build job = %v", b)
	}
	dp := src.GetJobs()[2]
	if dp.GetEnvironment().GetName() != "production" || dp.GetDeploy().GetFrom() != "build-web-image" {
		t.Errorf("deploy job = %v", dp)
	}
	if dp.GetRules()[0].GetWhen() != WhenManual {
		t.Errorf("rule when = %q", dp.GetRules()[0].GetWhen())
	}
}

func TestParseDefaults(t *testing.T) {
	src, err := Parse([]byte(`stages: [test]
unit:
  stage: test
  image: alpine
  script: [make]
  retry: { max: 1 }
  artifacts:
    paths: [out/]
`))
	if err != nil {
		t.Fatal(err)
	}
	j := src.GetJobs()[0]
	if j.GetWhen() != "" {
		t.Errorf("when is left empty in the file: got %q", j.GetWhen())
	}
	if len(j.GetRetry().GetWhen()) != 2 {
		t.Errorf("retry.when defaults to both infrastructure reasons: %v", j.GetRetry().GetWhen())
	}
	if j.GetArtifacts().GetExpireIn().AsDuration() != DefaultExpire {
		t.Errorf("artifacts expire in %v, want %v", j.GetArtifacts().GetExpireIn().AsDuration(), DefaultExpire)
	}
}

func TestParseAnchorsAndMerge(t *testing.T) {
	src, err := Parse([]byte(`stages: [test]
base: &base
  stage: test
  image: alpine
  script: [make]
unit:
  <<: *base
e2e:
  <<: *base
  script: [make e2e]
  tags: [linux]
`))
	if err != nil {
		t.Fatalf("anchors and merge keys are allowed: %v", err)
	}
	if len(src.GetJobs()) != 3 {
		t.Fatalf("want 3 jobs (base is a job too), got %d", len(src.GetJobs()))
	}
	unit, e2e := src.GetJobs()[1], src.GetJobs()[2]
	if unit.GetImage() != "alpine" || unit.GetStage() != "test" {
		t.Errorf("unit did not merge base: %v", unit)
	}
	if e2e.GetScript()[0] != "make e2e" {
		t.Errorf("an explicit key wins over a merged one: %v", e2e.GetScript())
	}
}

func TestParseErrors(t *testing.T) {
	const header = "stages: [build, test]\n"
	cases := []struct {
		name string
		file string
		want string // substring of the error, which names the key and line
	}{
		{"unknown job key names it and its line",
			header + "test:\n  stage: test\n  image: a\n  script: [b]\n  imge: x\n",
			`line 6: job "test": unknown key "imge"`},
		{"top-level include is refused",
			"include: other.yml\n" + header + "t:\n  stage: test\n  image: a\n  script: [b]\n",
			`line 1: "include" is not supported in shoal-ci.yml`},
		{"top-level workflow is refused",
			"workflow:\n  name: x\n" + header + "t:\n  stage: test\n  image: a\n  script: [b]\n",
			`line 1: "workflow" is not supported`},
		{"job extends is refused",
			header + "t:\n  extends: .base\n  stage: test\n  image: a\n  script: [b]\n",
			`"extends" is not supported`},
		{"before_script is refused",
			header + "t:\n  stage: test\n  image: a\n  script: [b]\n  before_script: [c]\n",
			`"before_script" is not supported`},
		{"needs inside the same stage",
			header + "a:\n  stage: test\n  image: x\n  script: [b]\nb:\n  stage: test\n  image: x\n  script: [b]\n  needs: [a]\n",
			`job "b": needs "a", which is in stage "test", not an earlier stage`},
		{"needs a later stage",
			header + "a:\n  stage: build\n  image: x\n  script: [b]\n  needs: [t]\nt:\n  stage: test\n  image: x\n  script: [b]\n",
			`job "a": needs "t", which is in stage "test", not an earlier stage`},
		{"needs a job that does not exist",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  needs: [nope]\n",
			`job "t": needs "nope", which is not a job in this file`},
		{"needs itself",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  needs: [t]\n",
			`job "t": needs itself`},
		{"stage not in stages",
			header + "t:\n  stage: deploy\n  image: x\n  script: [b]\n",
			`stage "deploy" is not in stages`},
		{"stage is required",
			header + "t:\n  image: x\n  script: [b]\n",
			`job "t": stage is required`},
		{"script job needs script",
			header + "t:\n  stage: test\n  image: x\n",
			`job "t": script is required`},
		{"script job needs image",
			header + "t:\n  stage: test\n  script: [b]\n",
			`job "t": image is required`},
		{"build job cannot have script",
			header + "t:\n  stage: test\n  build: web\n  image: x\n  script: [b]\n",
			"a build or deploy job cannot also have image or script"},
		{"when value is checked",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  when: sometimes\n",
			"when must be one of on_success, always, manual, never"},
		{"if with an operator outside the grammar",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  rules:\n    - if: branch != \"main\"\n",
			`"branch != \"main\"" is not supported`},
		{"if with an unknown term",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  rules:\n    - if: pipeline == \"x\"\n",
			`"pipeline == \"x\"" is not supported`},
		{"if with an unquoted branch",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  rules:\n    - if: branch == main\n",
			"branch must be compared with a quoted branch name"},
		{"rule without a condition",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  rules:\n    - when: manual\n",
			"a rule needs if or changes"},
		{"retry max above the limit",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  retry: { max: 3 }\n",
			"retry.max must be from 0 to 2"},
		{"retry on a script failure is refused",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  retry: { max: 1, when: [script_failure] }\n",
			"a script that exits non-zero is never retried"},
		{"secret from an external vault is refused",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  secrets: { TOKEN: npm-token }\n",
			"external vaults are not supported"},
		{"templated cache key is refused",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  cache: { key: 'npm-{hash}' }\n",
			"templated cache keys are not supported"},
		{"timeout must be a duration",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  timeout: soon\n",
			"timeout must be a positive duration"},
		{"artifact lifetime above 30 days",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  artifacts: { paths: [o/], expire_in: 60d }\n",
			"expire_in must be a duration from 1h to 30d"},
		{"environment approval other than required",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  environment: { name: prod, approval: maybe }\n",
			"environment.approval must be required"},
		{"deploy from must be in needs",
			header + "b:\n  stage: build\n  build: web\nt:\n  stage: test\n  image: x\n  script: [b]\n  deploy: { workload: web, from: b }\n",
			`deploy.from "b" must also be listed in needs`},
		{"deploy from must be a build job",
			header + "b:\n  stage: build\n  image: x\n  script: [b]\nt:\n  stage: test\n  needs: [b]\n  deploy: { workload: web, from: b }\n",
			`deploy.from "b" must be a build job`},
		{"job name must be a DNS label",
			header + "Test_Web:\n  stage: test\n  image: x\n  script: [b]\n",
			`job "Test_Web": use lowercase letters`},
		{"tag must be a label",
			header + "t:\n  stage: test\n  image: x\n  script: [b]\n  tags: [Linux AMD]\n",
			`tag "Linux AMD"`},
		{"duplicate stage",
			"stages: [a, a]\n",
			`stage "a" is listed twice`},
		{"no jobs",
			header,
			"the file has no jobs"},
		{"not YAML",
			"stages: [build\n",
			"not valid YAML"},
		{"a job must be a mapping",
			header + "t: [a]\n",
			`line 2: unknown key "t"`},
		{"stages are required",
			"t:\n  stage: test\n  image: x\n  script: [b]\n",
			"stages is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.file))
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseRefusesTooManyJobs(t *testing.T) {
	var b strings.Builder
	b.WriteString("stages: [test]\n")
	for i := range MaxJobs + 1 {
		fmt.Fprintf(&b, "j%d:\n  stage: test\n  image: x\n  script: [b]\n", i)
	}
	_, err := Parse([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "at most 100 per file") {
		t.Fatalf("want the job limit, got %v", err)
	}
}

func TestParseRefusesTooManyStages(t *testing.T) {
	_, err := Parse([]byte("stages: [a1, a2, a3, a4, a5, a6, a7, a8, a9, b1, b2]\nt:\n  stage: a1\n  image: x\n  script: [b]\n"))
	if err == nil || !strings.Contains(err.Error(), "at most 10 stages") {
		t.Fatalf("want the stage limit, got %v", err)
	}
}

func TestParseRejectsBillionLaughs(t *testing.T) {
	// Each level refers to the one above twice. Expanded naively this is
	// 2^n nodes; the alias depth limit keeps the parse bounded.
	var b strings.Builder
	b.WriteString("stages: [test]\nl0: &l0\n  stage: test\n  image: x\n  script: [b]\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "l%d: &l%d\n  x: [*l%d, *l%d]\n", i, i, i-1, i-1)
	}
	_, err := Parse([]byte(b.String()))
	if err == nil {
		t.Fatal("an alias bomb must not parse")
	}
}
