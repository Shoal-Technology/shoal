package resource

import "testing"

// FuzzDecode checks that arbitrary manifest bytes never panic the decoder,
// and that any accepted manifest yields only objects whose inner resource is
// set. The manifest is untrusted input from `shoal apply -f`.
func FuzzDecode(f *testing.F) {
	seeds := []string{
		"",
		"---\n",
		"# only a comment\n",
		"kind: Route\nmetadata:\n  name: web\nspec:\n  host: app.example.com\n  service: web\n  port: 8080\n",
		"apiVersion: shoal.dev/v1alpha1\nkind: Config\nmetadata:\n  name: app\nspec:\n  data:\n    cpu: 0.5\n    port: 5432\n    debug: true\n",
		"kind: Secret\nmetadata:\n  name: db\nspec:\n  data:\n    password: hunter2\n",
		"kind: Service\nmetadata:\n  name: web\n",
		"kind: Workload\nmetadata:\n  name: api\nspec:\n  resources:\n    cpu: 1\n---\nkind: Route\nmetadata:\n  name: api\n",
		"kind: Workload\nspec: [1, 2, 3]\n",
		"kind: Nope\n",
		"apiVersion: v2\nkind: Route\n",
		"kind: Route\nunknownField: 1\n",
		"&a kind: *a\n",
		"kind: \"\\u0000\"\n",
		"- just\n- a list\n",
		"kind: Config\nspec:\n  data: {a: [1, 2]}\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		objs, err := Decode(data)
		if err != nil {
			return
		}
		if len(objs) == 0 {
			t.Fatal("Decode returned no error and no objects")
		}
		for i, o := range objs {
			if Inner(o) == nil {
				t.Fatalf("object %d has no inner resource", i)
			}
		}
	})
}
