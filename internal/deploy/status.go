package deploy

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Readiness reports whether an object is ready, and a message for the
// progress line when it is not. A nil object (not created yet) is not ready.
// Kinds without a readiness notion are ready once they exist.
func Readiness(obj *v1alpha1.Object) (bool, string) {
	switch {
	case obj == nil:
		return false, "not found"
	case obj.GetWorkload() != nil:
		w := obj.GetWorkload()
		want := int32(1)
		if w.GetSpec().Replicas != nil {
			want = w.GetSpec().GetReplicas()
		}
		got := w.GetStatus().GetReadyReplicas()
		if got >= want {
			return true, ""
		}
		return false, fmt.Sprintf("%d/%d replicas ready", got, want)
	case obj.GetDatabase() != nil:
		st := obj.GetDatabase().GetStatus()
		if st.GetPhase() == "Ready" {
			return true, ""
		}
		msg := "phase " + orDefault(st.GetPhase(), "Pending")
		if st.GetMessage() != "" {
			msg += ": " + st.GetMessage()
		}
		return false, msg
	case obj.GetRoute() != nil:
		st := obj.GetRoute().GetStatus()
		certOK := st.GetCertState() == "" || st.GetCertState() == "CertIssued"
		if st.GetState() == "Ready" && certOK {
			return true, ""
		}
		msg := "state " + orDefault(st.GetState(), "Pending")
		if !certOK {
			msg += ", certificate " + st.GetCertState()
			if st.GetCertMessage() != "" {
				msg += ": " + st.GetCertMessage()
			}
		} else if st.GetMessage() != "" {
			msg += ": " + st.GetMessage()
		}
		return false, msg
	}
	return true, ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Row is one line of app status.
type Row struct {
	Kind    string
	Name    string
	State   string
	Message string
}

// StatusRows describes objs (in apply order). A Secret shows its key count
// only: values are never shown.
func StatusRows(objs []*v1alpha1.Object) []Row {
	rows := make([]Row, 0, len(objs))
	for _, obj := range objs {
		ref, _, _ := Describe(obj)
		row := Row{Kind: ref.Kind, Name: ref.Name}
		switch {
		case obj.GetWorkload() != nil:
			_, msg := Readiness(obj)
			w := obj.GetWorkload()
			want := int32(1)
			if w.GetSpec().Replicas != nil {
				want = w.GetSpec().GetReplicas()
			}
			row.State = fmt.Sprintf("%d/%d ready", w.GetStatus().GetReadyReplicas(), want)
			row.Message = msg
		case obj.GetDatabase() != nil:
			row.State = orDefault(obj.GetDatabase().GetStatus().GetPhase(), "Pending")
			row.Message = obj.GetDatabase().GetStatus().GetMessage()
		case obj.GetRoute() != nil:
			st := obj.GetRoute().GetStatus()
			row.State = orDefault(st.GetState(), "Pending")
			row.Message = st.GetMessage()
			if st.GetCertState() != "" {
				row.Message = joinMsg(row.Message, "certificate "+st.GetCertState()+" "+st.GetCertMessage())
			}
		case obj.GetVolume() != nil:
			row.State = orDefault(obj.GetVolume().GetStatus().GetState(), "Pending")
			row.Message = obj.GetVolume().GetStatus().GetMessage()
		case obj.GetSecret() != nil:
			n := len(obj.GetSecret().GetSpec().GetData())
			if n == 0 {
				row.State = "empty"
			} else {
				row.State = fmt.Sprintf("%d keys set", n)
			}
		default:
			row.State = "-"
		}
		rows = append(rows, row)
	}
	return rows
}

func joinMsg(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// URLFor returns the address of the app: https://<host> from its Route, else
// the service address <app>.shoal:80, else "" when neither exists.
func URLFor(app string, objs []*v1alpha1.Object) string {
	hasService := false
	for _, obj := range objs {
		if r := obj.GetRoute(); r != nil && r.GetSpec().GetHost() != "" {
			return "https://" + r.GetSpec().GetHost()
		}
		if obj.GetService() != nil {
			hasService = true
		}
	}
	if hasService {
		return app + ".shoal:80"
	}
	return ""
}

// Summary is the end-of-deploy report.
type Summary struct {
	App           string
	URL           string
	Replicas      int32
	ReadyReplicas int32
	HasDatabase   bool
	DatabaseReady bool
	Elapsed       time.Duration
}

// Summarize reads the current state of the app's workload, database and
// routes. desired is the manifest, which says which of them exist.
func Summarize(ctx context.Context, ctl v1alpha1.ControlClient, app string, desired []*v1alpha1.Object) (Summary, error) {
	s := Summary{App: app, Replicas: 1}
	var current []*v1alpha1.Object
	for _, ref := range WaitTargets(desired) {
		obj, err := get(ctx, ctl, ref)
		if err != nil {
			return s, err
		}
		current = append(current, obj)
	}
	for _, obj := range current {
		switch {
		case obj.GetWorkload() != nil:
			w := obj.GetWorkload()
			if w.GetSpec().Replicas != nil {
				s.Replicas = w.GetSpec().GetReplicas()
			}
			s.ReadyReplicas = w.GetStatus().GetReadyReplicas()
		case obj.GetDatabase() != nil:
			s.HasDatabase = true
			ok, _ := Readiness(obj)
			s.DatabaseReady = ok
		}
	}
	s.URL = URLFor(app, desired)
	return s, nil
}

// PrintSummary writes the summary block that ends a deploy.
func PrintSummary(w io.Writer, s Summary) {
	if s.Elapsed > 0 {
		fmt.Fprintf(w, "\n%s ready in %s\n", s.App, s.Elapsed.Round(time.Second))
	}
	fmt.Fprintf(w, "app       %s\n", s.App)
	fmt.Fprintf(w, "replicas  %d/%d ready\n", s.ReadyReplicas, s.Replicas)
	url := s.URL
	if url == "" {
		url = "none (no domain or service)"
	}
	fmt.Fprintf(w, "url       %s\n", url)
	if s.HasDatabase {
		fmt.Fprintf(w, "database  ready: %s\n", yesNo(s.DatabaseReady))
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
