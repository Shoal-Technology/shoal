package appspec

import (
	"os"
	"testing"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/resource"
)

// A Database emitted from an app spec passes the server's Prepare, the same
// check Apply runs on every object, so deploy can send it unchanged.
func TestEmittedDatabasePassesPrepare(t *testing.T) {
	data, err := os.ReadFile("testdata/web_db_domain.yaml")
	if err != nil {
		t.Fatal(err)
	}
	app, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Expand(app)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, o := range objs {
		if o.GetDatabase() == nil {
			continue
		}
		found++
		kind, name, _, err := resource.Prepare(o)
		if err != nil {
			t.Fatalf("Prepare(emitted Database): %v", err)
		}
		if kind != v1alpha1.KindDatabase || name != "blog-db" {
			t.Errorf("Prepare = %s %s, want Database blog-db", kind, name)
		}
	}
	if found != 1 {
		t.Fatalf("%d Databases emitted, want 1", found)
	}
}
