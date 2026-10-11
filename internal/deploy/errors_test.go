package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// A terminal error (ACME terms not accepted, E06) fails the wait at once,
// well before its timeout, and names the code and the hint.
func TestWaitFailsFastOnTerminalError(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	f.liveObj(t, Ref{v1alpha1.KindRoute, "blog"}).GetRoute().Status = &v1alpha1.RouteStatus{
		State:       "Pending",
		CertState:   "CertError",
		CertMessage: "ACME terms not accepted; start the server with --acme-accept-tos",
	}

	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	err := Wait(wctx, f, WaitTargets(example(t)), time.Millisecond, &out)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Wait took %s; a terminal error must fail fast", took)
	}
	var fe *FailedError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want *FailedError", err)
	}
	if fe.Entry.Code != "E06" {
		t.Errorf("code = %s, want E06", fe.Entry.Code)
	}
	if !strings.Contains(err.Error(), "E06: ACME terms not accepted — ") {
		t.Errorf("error %q does not carry the E06 line", err)
	}
	if !strings.Contains(out.String(), "failed     route/blog: E06") {
		t.Errorf("no failed line:\n%s", out.String())
	}
}

// A transient error (HTTP-01 failing, E07) keeps the wait going until the
// timeout, and the timeout error carries the code.
func TestWaitKeepsWaitingOnTransientError(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	f.liveObj(t, Ref{v1alpha1.KindRoute, "blog"}).GetRoute().Status = &v1alpha1.RouteStatus{
		State:       "Pending",
		CertState:   "CertError",
		CertMessage: "certificate for blog.example.com: HTTP-01 challenge failed: connection refused",
	}

	wctx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := Wait(wctx, f, WaitTargets(example(t)), 5*time.Millisecond, &out)
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want *NotReadyError (the state is transient)", err)
	}
	if strings.Contains(out.String(), "failed     ") {
		t.Errorf("transient state reported as failed:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "E07: HTTP-01 challenge fails, or rate limited — ") {
		t.Errorf("timeout error %q does not carry the E07 line", err)
	}
}

// The deploy summary and app status show the code of a matched state.
func TestDeploySummaryAndStatusShowCode(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	f.liveObj(t, Ref{v1alpha1.KindRoute, "blog"}).GetRoute().Status = &v1alpha1.RouteStatus{
		State:       "Pending",
		CertState:   "CertError",
		CertMessage: "rate limited until 2026-10-12",
	}

	var out bytes.Buffer
	opts := deployOpts(example(t), &out) // Wait is zero: no wait, straight to the summary
	if _, err := Run(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "issue     E07: ") {
		t.Errorf("summary does not show the code:\n%s", out.String())
	}

	objs, err := AppObjects(ctx, f, "blog")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range StatusRows(objs) {
		if r.Kind == v1alpha1.KindRoute && r.Code == "E07" && strings.HasPrefix(r.Issue, "E07: ") {
			found = true
		}
	}
	if !found {
		t.Error("app status row for the route has no E07 code")
	}
}
