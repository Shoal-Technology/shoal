package resource

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Secret types, named after the core/v1 SecretType constants (K-28). An empty
// type reads as Opaque, so Secrets written before types existed stay valid.
const (
	SecretTypeOpaque           = "Opaque"
	SecretTypeDockerConfigJSON = "kubernetes.io/dockerconfigjson"
	SecretTypeTLS              = "kubernetes.io/tls"
	SecretTypeBasicAuth        = "kubernetes.io/basic-auth"
	SecretTypeSSHAuth          = "kubernetes.io/ssh-auth"

	// DockerConfigKey holds the docker config.json of a dockerconfigjson Secret.
	DockerConfigKey = ".dockerconfigjson"
	// SSHPrivateKeyKey holds the private key of an ssh-auth Secret.
	SSHPrivateKeyKey = "ssh-privatekey"
	// KnownHostsKey optionally holds the known_hosts lines of an ssh-auth Secret.
	KnownHostsKey = "known_hosts"
)

// SecretTypes lists the accepted types, for messages.
var SecretTypes = []string{SecretTypeOpaque, SecretTypeDockerConfigJSON, SecretTypeTLS, SecretTypeBasicAuth, SecretTypeSSHAuth}

// ValidateSecret checks the data of a Secret against its type. What can be
// checked offline is checked here, so a bad Secret is refused at apply time
// and does not fail later on a node.
func ValidateSecret(name string, s *v1alpha1.SecretSpec) error {
	errs := []error{validateData(name, s.GetData())}
	switch t := s.GetType(); t {
	case "", SecretTypeOpaque:
	case SecretTypeDockerConfigJSON:
		if _, err := ParseDockerConfig(s.GetData()); err != nil {
			errs = append(errs, fmt.Errorf("type %s: %w", t, err))
		}
	case SecretTypeTLS:
		if _, _, err := ParseRouteSecret(s.GetData()); err != nil {
			errs = append(errs, fmt.Errorf("type %s: %w", t, err))
		}
	case SecretTypeBasicAuth:
		errs = append(errs, requireKeys(t, s.GetData(), "username", "password"))
	case SecretTypeSSHAuth:
		errs = append(errs, requireKeys(t, s.GetData(), SSHPrivateKeyKey))
		if k := s.GetData()[SSHPrivateKeyKey]; k != "" && !strings.Contains(k, "PRIVATE KEY-----") {
			errs = append(errs, fmt.Errorf("type %s: %s is not a private key", t, SSHPrivateKeyKey))
		}
	default:
		errs = append(errs, fmt.Errorf("type: %q is not supported (use %s)", t, strings.Join(SecretTypes, ", ")))
	}
	return errors.Join(errs...)
}

// ParseDockerConfig reads the registry logins of a dockerconfigjson Secret.
// Each entry of the auths map gives either auth (base64 of user:password) or
// username and password. The result is sorted by host.
// Error messages name hosts and fields, never the credentials.
func ParseDockerConfig(data map[string]string) ([]*v1alpha1.RegistryCredential, error) {
	raw, ok := data[DockerConfigKey]
	if !ok {
		return nil, fmt.Errorf("the Secret must hold %s", DockerConfigKey)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("%s is not a docker config: %w", DockerConfigKey, err)
	}
	if len(cfg.Auths) == 0 {
		return nil, fmt.Errorf("%s lists no registry under auths", DockerConfigKey)
	}
	var out []*v1alpha1.RegistryCredential
	seen := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(cfg.Auths)) {
		e := cfg.Auths[key]
		host := RegistryHost(key)
		if host == "" {
			return nil, fmt.Errorf("auths %q: empty registry name", key)
		}
		if prev, dup := seen[host]; dup {
			return nil, fmt.Errorf("auths %q and %q name the same registry", prev, key)
		}
		seen[host] = key
		user, pass := e.Username, e.Password
		if e.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				return nil, fmt.Errorf("auths %q: auth is not base64", key)
			}
			var found bool
			if user, pass, found = strings.Cut(string(decoded), ":"); !found {
				return nil, fmt.Errorf("auths %q: auth does not decode to user:password", key)
			}
		}
		if user == "" || pass == "" {
			return nil, fmt.Errorf("auths %q: give auth, or username and password", key)
		}
		out = append(out, &v1alpha1.RegistryCredential{Host: host, Username: user, Password: pass})
	}
	return out, nil
}

// RegistryHost is the host of an auths key, as the container runtime asks for
// credentials: the scheme and any path are dropped, and every Docker Hub key
// maps to registry-1.docker.io.
func RegistryHost(key string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "registry-1.docker.io"
	}
	return h
}

// requireKeys reports the keys of a typed Secret that are missing or empty.
// The error names keys, never values.
func requireKeys(t string, data map[string]string, keys ...string) error {
	var missing []string
	for _, k := range keys {
		if data[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("type %s: must hold %s", t, strings.Join(missing, ", "))
	}
	return nil
}

// validatePullSecrets checks the image pull Secret names of a workload spec.
// Whether each Secret exists and has the dockerconfigjson type is decided on
// the node's snapshot, as for env references.
func validatePullSecrets(s *v1alpha1.WorkloadSpec, bad func(field, format string, args ...any)) {
	seen := map[string]bool{}
	for i, name := range s.GetImagePullSecrets() {
		f := fmt.Sprintf("spec.imagePullSecrets[%d]", i)
		switch {
		case !ValidName(name, 63):
			bad(f, "invalid Secret name %q", name)
		case seen[name]:
			bad(f, "duplicate %q", name)
		}
		seen[name] = true
	}
}
