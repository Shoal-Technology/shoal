package agent

import (
	"encoding/base64"
	"slices"
	"testing"
)

// TestRedactionsCoverSecretsAndTheirBase64: a script that prints a secret, or
// its base64 form, must not reach the logs. Empty values are skipped.
func TestRedactionsCoverSecretsAndTheirBase64(t *testing.T) {
	got := redactions([]string{"token-one", ""}, "password-two")
	for _, v := range []string{"token-one", "password-two"} {
		if !slices.Contains(got, v) {
			t.Errorf("redactions = %v, missing %q", got, v)
		}
		if enc := base64.StdEncoding.EncodeToString([]byte(v)); !slices.Contains(got, enc) {
			t.Errorf("redactions = %v, missing the base64 form of %q", got, v)
		}
	}
	if slices.Contains(got, "") {
		t.Errorf("redactions = %v, want no empty value", got)
	}
}
