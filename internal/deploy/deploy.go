package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/appspec"
	"github.com/Shoal-Technology/shoal/internal/errcat"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

// ErrAborted is returned when the user declines a confirmation.
var ErrAborted = errors.New("aborted")

// Change is one planned action on an object: create, update or unchanged.
type Change struct {
	Ref    Ref
	Action string
	Reason string
}

// normalizedSpec returns the spec the server would store for obj, with the
// server's defaults applied, so that a re-deploy compares equal. Database has
// no case in resource.Prepare yet; its spec is compared as written.
func normalizedSpec(obj *v1alpha1.Object) (proto.Message, error) {
	ref, _, spec := Describe(obj)
	_, _, s, err := resource.Prepare(obj)
	if err != nil {
		if ref.Kind == v1alpha1.KindDatabase {
			return spec, nil
		}
		return nil, err
	}
	return s, nil
}

// Plan compares the desired objects of app with the live state, in the order
// given. It fails when a desired object's name belongs to an object that is
// not labelled app=<app> (ADR 0017, E13), so deploy never takes it over.
func Plan(app string, desired []*v1alpha1.Object, live Live) ([]Change, error) {
	var (
		changes []Change
		errs    []error
	)
	for _, obj := range desired {
		ref, meta, _ := Describe(obj)
		spec, err := normalizedSpec(obj)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ref, err))
			continue
		}
		cur, exists := live[ref]
		if !exists {
			changes = append(changes, Change{Ref: ref, Action: "create"})
			continue
		}
		if labelsOf(cur)[appspec.LabelApp] != app {
			errs = append(errs, fmt.Errorf("%s already exists and is not labelled %s=%s; refusing to overwrite it (ADR 0017, E13)",
				ref, appspec.LabelApp, app))
			continue
		}
		var why []string
		if !proto.Equal(spec, fieldOf(innerOf(cur), "spec")) {
			why = append(why, "spec changed")
		}
		if !maps.Equal(meta.GetLabels(), labelsOf(cur)) {
			why = append(why, "labels changed")
		}
		if len(why) == 0 {
			changes = append(changes, Change{Ref: ref, Action: "unchanged"})
		} else {
			changes = append(changes, Change{Ref: ref, Action: "update", Reason: strings.Join(why, ", ")})
		}
	}
	return changes, errors.Join(errs...)
}

// secretObject builds the Secret <app>-secrets with the given data and labels.
func secretObject(app string, data map[string]string, labels map[string]string) *v1alpha1.Object {
	return &v1alpha1.Object{Kind: &v1alpha1.Object_Secret{Secret: &v1alpha1.Secret{
		Metadata: &v1alpha1.ObjectMeta{Name: appspec.SecretsName(app), Labels: labels},
		Spec:     &v1alpha1.SecretSpec{Data: data},
	}}}
}

// withSecret adds the app's Secret to the desired objects when it is
// missing. A Secret that exists keeps its data: only its labels are set.
func withSecret(app string, desired []*v1alpha1.Object, live Live) []*v1alpha1.Object {
	ref := Ref{Kind: v1alpha1.KindSecret, Name: appspec.SecretsName(app)}
	labels := map[string]string{appspec.LabelApp: app}
	var data map[string]string
	if cur, ok := live[ref]; ok {
		maps.Copy(labels, labelsOf(cur))
		labels[appspec.LabelApp] = app
		data = maps.Clone(cur.GetSecret().GetSpec().GetData())
	}
	out := append(slices.Clone(desired), secretObject(app, data, labels))
	SortObjects(out)
	return out
}

// Options control Run.
type Options struct {
	App     string
	Desired []*v1alpha1.Object // from appspec.Expand
	DryRun  bool
	// Prune deletes labelled objects that are no longer in the manifest.
	Prune bool
	// PruneData also deletes Volumes, Databases and Secrets among them.
	PruneData bool
	// Wait is how long to wait for readiness; zero skips the wait.
	Wait time.Duration
	// Poll is the interval between readiness checks (default 2s).
	Poll time.Duration
	// Out receives the plan, progress and summary. Nil discards them.
	Out io.Writer
}

// Result is what a deploy did, or with DryRun what it would do.
type Result struct {
	Changes []Change
	// Orphans are labelled objects that the manifest no longer has.
	Orphans []Ref
	// Deleted are the orphans removed by Prune.
	Deleted []Ref
	// Kept are the orphans left in place.
	Kept    []Ref
	Summary Summary
}

// orphanRefs lists the live objects labelled app that desired does not name,
// in deletion order: the workloads before the data they use.
func orphanRefs(app string, desired []*v1alpha1.Object, live Live) []Ref {
	want := map[Ref]bool{}
	for _, obj := range desired {
		ref, _, _ := Describe(obj)
		want[ref] = true
	}
	var out []Ref
	for ref, obj := range live {
		if !want[ref] && labelsOf(obj)[appspec.LabelApp] == app && labelsOf(obj)[resource.OwnedByLabel] == "" {
			out = append(out, ref)
		}
	}
	slices.SortFunc(out, func(a, b Ref) int {
		if c := rank(b.Kind) - rank(a.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// Run plans and, unless DryRun is set, applies the app's objects in one
// batch, prunes orphans when asked and waits for readiness. It prints the
// plan, progress lines and a summary to opts.Out.
func Run(ctx context.Context, ctl v1alpha1.ControlClient, opts Options) (Result, error) {
	var res Result
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	start := time.Now()
	live, err := ListLive(ctx, ctl)
	if err != nil {
		return res, err
	}
	desired := withSecret(opts.App, opts.Desired, live)
	if res.Changes, err = Plan(opts.App, desired, live); err != nil {
		return res, err
	}
	res.Orphans = orphanRefs(opts.App, desired, live)
	for _, ref := range res.Orphans {
		switch {
		case !opts.Prune:
			res.Kept = append(res.Kept, ref)
		case isData(ref.Kind) && !opts.PruneData:
			res.Kept = append(res.Kept, ref)
		default:
			res.Deleted = append(res.Deleted, ref)
		}
	}
	printPlan(out, res, opts)
	if opts.DryRun {
		return res, nil
	}

	resp, err := ctl.Apply(ctx, &v1alpha1.ApplyRequest{Objects: desired})
	if err != nil {
		return res, err
	}
	created, configured := 0, 0
	for _, r := range resp.GetResults() {
		switch r.GetAction() {
		case "created":
			created++
		case "configured":
			configured++
		}
	}
	fmt.Fprintf(out, "applied %d objects (%d created, %d configured)\n", len(resp.GetResults()), created, configured)

	for _, ref := range res.Deleted {
		if _, err := ctl.Delete(ctx, &v1alpha1.DeleteRequest{Kind: ref.Kind, Name: ref.Name}); err != nil {
			return res, fmt.Errorf("delete %s: %w", ref, err)
		}
		fmt.Fprintf(out, "deleted %s\n", ref)
	}

	if opts.Wait > 0 {
		wctx, cancel := context.WithTimeout(ctx, opts.Wait)
		err := Wait(wctx, ctl, WaitTargets(desired), opts.Poll, out)
		cancel()
		if err != nil {
			return res, err
		}
	}
	if res.Summary, err = Summarize(ctx, ctl, opts.App, desired); err != nil {
		return res, err
	}
	if opts.Wait > 0 {
		res.Summary.Elapsed = time.Since(start)
	}
	PrintSummary(out, res.Summary)
	return res, nil
}

// printPlan writes one line per object: its action, then the orphans.
func printPlan(w io.Writer, res Result, opts Options) {
	for _, c := range res.Changes {
		line := fmt.Sprintf("%-10s %s", c.Action, c.Ref)
		if c.Reason != "" {
			line += " (" + c.Reason + ")"
		}
		fmt.Fprintln(w, line)
	}
	for _, ref := range res.Orphans {
		action := "orphan"
		if slices.Contains(res.Deleted, ref) {
			action = "delete"
		}
		note := "not in the manifest, left in place (use --prune to delete it)"
		switch {
		case action == "delete":
			note = "not in the manifest"
		case isData(ref.Kind):
			note = "not in the manifest, kept: data is only deleted with --prune-data"
		}
		fmt.Fprintf(w, "%-10s %s (%s)\n", action, ref, note)
	}
}

// WaitTargets returns the objects that a deploy waits for: the workload, and
// the database and route when the manifest has them.
func WaitTargets(desired []*v1alpha1.Object) []Ref {
	var out []Ref
	for _, obj := range desired {
		ref, _, _ := Describe(obj)
		switch ref.Kind {
		case v1alpha1.KindWorkload, v1alpha1.KindDatabase, v1alpha1.KindRoute:
			out = append(out, ref)
		}
	}
	return out
}

// Pending is an object that a wait ended with, and why it is not ready.
// Entry is set when the status matches a catalogue error.
type Pending struct {
	Ref     Ref
	Message string
	Entry   *errcat.Entry
}

// NotReadyError names the objects still not ready when a wait ended.
type NotReadyError struct {
	Pending []Pending
}

func (e *NotReadyError) Error() string {
	parts := make([]string, 0, len(e.Pending))
	for _, p := range e.Pending {
		part := fmt.Sprintf("%s: %s", p.Ref, p.Message)
		if p.Entry != nil {
			part += " [" + p.Entry.Line() + "]"
		}
		parts = append(parts, part)
	}
	return "timed out waiting for readiness; not ready: " + strings.Join(parts, "; ")
}

// FailedError is returned by Wait at once when an object is in a terminal
// error state (errcat.Terminal). Waiting longer cannot fix it.
type FailedError struct {
	Ref     Ref
	Message string
	Entry   errcat.Entry
}

func (e *FailedError) Error() string {
	return fmt.Sprintf("%s failed: %s (%s)", e.Ref, e.Entry.Line(), e.Message)
}

// Wait polls the targets until each one is ready, or ctx ends. It prints a
// line when an object becomes ready or its status message changes, and
// returns a *NotReadyError naming what is still pending when ctx ends.
func Wait(ctx context.Context, ctl v1alpha1.ControlClient, targets []Ref, poll time.Duration, out io.Writer) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if out == nil {
		out = io.Discard
	}
	last := map[Ref]string{}
	pending := make([]Pending, 0, len(targets))
	for _, ref := range targets {
		pending = append(pending, Pending{Ref: ref, Message: "not checked yet"})
	}
	for {
		var next []Pending
		for _, ref := range targets {
			obj, err := get(ctx, ctl, ref)
			if err != nil {
				if ctx.Err() != nil {
					return &NotReadyError{Pending: pending}
				}
				return err
			}
			ready, msg := Readiness(obj)
			if ready {
				if last[ref] != "ready" {
					fmt.Fprintf(out, "ready      %s\n", ref)
					last[ref] = "ready"
				}
				continue
			}
			entry, known := Reason(obj)
			if known && errcat.Terminal(*entry) {
				fmt.Fprintf(out, "failed     %s: %s\n", ref, entry.Line())
				return &FailedError{Ref: ref, Message: msg, Entry: *entry}
			}
			if last[ref] != msg {
				fmt.Fprintf(out, "waiting    %s: %s\n", ref, msg)
				last[ref] = msg
			}
			p := Pending{Ref: ref, Message: msg}
			if known {
				p.Entry = entry
			}
			next = append(next, p)
		}
		if len(next) == 0 {
			return nil
		}
		pending = next
		select {
		case <-ctx.Done():
			return &NotReadyError{Pending: pending}
		case <-time.After(poll):
		}
	}
}

// Delete plan for an app: what is removed and what is kept.
type DeletePlan struct {
	Delete []Ref
	Keep   []Ref
}

// DeleteOptions control DeleteApp.
type DeleteOptions struct {
	// Data also deletes the Volumes, Database and Secret of the app.
	Data bool
	// Confirm sees the plan before anything is deleted. A false result
	// aborts with ErrAborted. Nil confirms.
	Confirm func(DeletePlan) bool
}

// DeleteApp deletes every object labelled app=<app>, except the data objects
// unless opts.Data is set. The plan is shown to Confirm first.
func DeleteApp(ctx context.Context, ctl v1alpha1.ControlClient, app string, opts DeleteOptions) (DeletePlan, error) {
	var plan DeletePlan
	objs, err := AppObjects(ctx, ctl, app)
	if err != nil {
		return plan, err
	}
	if len(objs) == 0 {
		return plan, fmt.Errorf("no objects labelled %s=%s", appspec.LabelApp, app)
	}
	// Delete the workloads and routes before the data they use.
	for i := len(objs) - 1; i >= 0; i-- {
		ref, _, _ := Describe(objs[i])
		if isData(ref.Kind) && !opts.Data {
			plan.Keep = append(plan.Keep, ref)
		} else {
			plan.Delete = append(plan.Delete, ref)
		}
	}
	if opts.Confirm != nil && !opts.Confirm(plan) {
		return plan, ErrAborted
	}
	for _, ref := range plan.Delete {
		if _, err := ctl.Delete(ctx, &v1alpha1.DeleteRequest{Kind: ref.Kind, Name: ref.Name}); err != nil {
			return plan, fmt.Errorf("delete %s: %w", ref, err)
		}
	}
	return plan, nil
}

// SetSecret writes values into the app's Secret, creating it when missing.
// Keys not in values keep their data. It returns the key names written, and
// never the values.
func SetSecret(ctx context.Context, ctl v1alpha1.ControlClient, app string, values map[string]string) ([]string, error) {
	ref := Ref{Kind: v1alpha1.KindSecret, Name: appspec.SecretsName(app)}
	cur, err := get(ctx, ctl, ref)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{appspec.LabelApp: app}
	data := map[string]string{}
	if cur != nil {
		if labelsOf(cur)[appspec.LabelApp] != app {
			return nil, fmt.Errorf("%s already exists and is not labelled %s=%s; refusing to change it", ref, appspec.LabelApp, app)
		}
		maps.Copy(labels, labelsOf(cur))
		labels[appspec.LabelApp] = app
		maps.Copy(data, cur.GetSecret().GetSpec().GetData())
	}
	maps.Copy(data, values)
	if _, err := ctl.Apply(ctx, &v1alpha1.ApplyRequest{Objects: []*v1alpha1.Object{secretObject(app, data, labels)}}); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseAssignments reads KEY=VALUE arguments whose keys become environment
// variables (app env, and the app secret keys its spec lists). A value of "-"
// is read from stdin, once, without its final newline. Errors never include a
// value.
func ParseAssignments(args []string, stdin io.Reader) (map[string]string, error) {
	return parseAssignments(args, stdin, keyPattern.MatchString,
		"invalid key (letters, digits and underscores, not starting with a digit)")
}

// ParseSecretAssignments reads KEY=VALUE arguments for the data of a Secret.
// Keys follow resource.ValidDataKey, so webhook-secret is valid. Values are
// read as in ParseAssignments.
func ParseSecretAssignments(args []string, stdin io.Reader) (map[string]string, error) {
	return parseAssignments(args, stdin, resource.ValidDataKey,
		"invalid key (letters, digits, '-', '_', '.'; at most 253 characters; not '.' or '..')")
}

func parseAssignments(args []string, stdin io.Reader, validKey func(string) bool, keyRule string) (map[string]string, error) {
	values := map[string]string{}
	var errs []error
	stdinUsed := false
	for i, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			errs = append(errs, fmt.Errorf("argument %d: want KEY=VALUE", i+1))
			continue
		}
		if !validKey(k) {
			errs = append(errs, fmt.Errorf("argument %d: %s", i+1, keyRule))
			continue
		}
		if _, dup := values[k]; dup {
			errs = append(errs, fmt.Errorf("key %s given twice", k))
			continue
		}
		if v == "-" {
			if stdinUsed || stdin == nil {
				errs = append(errs, fmt.Errorf("key %s: %s", k, "a value from stdin needs stdin, and only one key can use it"))
				continue
			}
			stdinUsed = true
			b, err := io.ReadAll(stdin)
			if err != nil {
				errs = append(errs, fmt.Errorf("key %s: read stdin: %w", k, err))
				continue
			}
			v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		}
		values[k] = v
	}
	return values, errors.Join(errs...)
}
