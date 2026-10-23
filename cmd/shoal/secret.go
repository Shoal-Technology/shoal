package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/deploy"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

// secretFlags pick a Secret's type and where its data comes from.
type secretFlags struct {
	typ            string // opaque, dockerconfigjson or tls
	fromDockerCfg  string // dockerconfigjson: an existing docker config.json
	dockerServer   string // dockerconfigjson: registry host, such as ghcr.io
	dockerUsername string // dockerconfigjson: user name for --docker-server
	passwordStdin  bool   // dockerconfigjson: read the password from stdin
	cert, key      string // tls: PEM files
}

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Create typed Secrets",
	}
	cmd.AddCommand(newSecretCreateCmd())
	return cmd
}

func newSecretCreateCmd() *cobra.Command {
	var (
		cf    clientFlags
		flags secretFlags
	)
	cmd := &cobra.Command{
		Use:   "create NAME [KEY=VALUE...]",
		Short: "Create or replace a Secret of type opaque, dockerconfigjson or tls",
		Long: `Create a Secret. The type decides what its data must hold:

  opaque (default)   KEY=VALUE pairs; KEY=- reads the value from stdin
  dockerconfigjson   registry logins for an app's imagePullSecrets: from
                     --from-docker-config FILE, or from --docker-server,
                     --docker-username and the password on stdin
  tls                --cert and --key, PEM files

Values are never printed. An existing Secret is replaced, as with apply.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			obj, err := buildSecret(args[0], flags, args[1:], cmd.InOrStdin())
			if err != nil {
				return err
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.Apply(ctx, &v1alpha1.ApplyRequest{Objects: []*v1alpha1.Object{obj}})
			for _, r := range resp.GetResults() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s %s\n", strings.ToLower(r.Kind), r.Name, r.Action)
			}
			return err
		},
	}
	cf.register(cmd.Flags())
	fs := cmd.Flags()
	fs.StringVar(&flags.typ, "type", "opaque", "opaque, dockerconfigjson or tls")
	fs.StringVar(&flags.fromDockerCfg, "from-docker-config", "", "dockerconfigjson: an existing docker config.json file")
	fs.StringVar(&flags.dockerServer, "docker-server", "", "dockerconfigjson: registry host, such as ghcr.io")
	fs.StringVar(&flags.dockerUsername, "docker-username", "", "dockerconfigjson: user name at --docker-server")
	fs.BoolVar(&flags.passwordStdin, "docker-password-stdin", false, "dockerconfigjson: read the password from stdin")
	fs.StringVar(&flags.cert, "cert", "", "tls: PEM certificate chain file")
	fs.StringVar(&flags.key, "key", "", "tls: PEM private key file")
	return cmd
}

// buildSecret turns the flags and arguments into a Secret that passes the
// server's rules. Files are read here, so a missing file or an invalid Secret
// fails before any connection is made.
func buildSecret(name string, f secretFlags, args []string, stdin io.Reader) (*v1alpha1.Object, error) {
	dockerFlags := f.fromDockerCfg != "" || f.dockerServer != "" || f.dockerUsername != "" || f.passwordStdin
	tlsFlags := f.cert != "" || f.key != ""
	var (
		typ  string
		data map[string]string
		err  error
	)
	switch f.typ {
	case "opaque":
		typ = resource.SecretTypeOpaque
		if dockerFlags || tlsFlags {
			return nil, fmt.Errorf("--type opaque takes KEY=VALUE arguments, not docker or tls flags")
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("an opaque Secret needs at least one KEY=VALUE")
		}
		data, err = deploy.ParseSecretAssignments(args, stdin)
	case "dockerconfigjson":
		typ = resource.SecretTypeDockerConfigJSON
		if len(args) > 0 || tlsFlags {
			return nil, fmt.Errorf("--type dockerconfigjson takes docker flags, not KEY=VALUE or tls flags")
		}
		data, err = dockerConfigData(f, stdin)
	case "tls":
		typ = resource.SecretTypeTLS
		if len(args) > 0 || dockerFlags {
			return nil, fmt.Errorf("--type tls takes --cert and --key, not KEY=VALUE or docker flags")
		}
		data, err = tlsData(f.cert, f.key)
	default:
		return nil, fmt.Errorf("unknown --type %q (use opaque, dockerconfigjson or tls)", f.typ)
	}
	if err != nil {
		return nil, err
	}
	obj := &v1alpha1.Object{Kind: &v1alpha1.Object_Secret{Secret: &v1alpha1.Secret{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec:     &v1alpha1.SecretSpec{Type: typ, Data: data},
	}}}
	if _, _, _, err := resource.Prepare(obj); err != nil {
		return nil, fmt.Errorf("secret %q: %w", name, err)
	}
	return obj, nil
}

// dockerConfigData builds the .dockerconfigjson data, either from a file as
// given or from one registry login. The password is read only from stdin, so
// it never reaches the process list or the shell history.
func dockerConfigData(f secretFlags, stdin io.Reader) (map[string]string, error) {
	if f.fromDockerCfg != "" {
		if f.dockerServer != "" || f.dockerUsername != "" || f.passwordStdin {
			return nil, fmt.Errorf("--from-docker-config cannot be combined with --docker-server, --docker-username or --docker-password-stdin")
		}
		raw, err := os.ReadFile(f.fromDockerCfg)
		if err != nil {
			return nil, err
		}
		return map[string]string{resource.DockerConfigKey: string(raw)}, nil
	}
	if f.dockerServer == "" || f.dockerUsername == "" || !f.passwordStdin {
		return nil, fmt.Errorf("give --from-docker-config FILE, or --docker-server, --docker-username and --docker-password-stdin")
	}
	if strings.Contains(f.dockerUsername, ":") {
		return nil, fmt.Errorf("--docker-username cannot contain ':'")
	}
	pass, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(string(pass), "\r\n")
	auth := base64.StdEncoding.EncodeToString([]byte(f.dockerUsername + ":" + password))
	cfg, err := json.Marshal(map[string]any{
		"auths": map[string]any{f.dockerServer: map[string]string{"auth": auth}},
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{resource.DockerConfigKey: string(cfg)}, nil
}

// tlsData reads the certificate chain and key files of a tls Secret.
func tlsData(cert, key string) (map[string]string, error) {
	if cert == "" || key == "" {
		return nil, fmt.Errorf("--type tls needs --cert and --key")
	}
	c, err := os.ReadFile(cert)
	if err != nil {
		return nil, err
	}
	k, err := os.ReadFile(key)
	if err != nil {
		return nil, err
	}
	return map[string]string{resource.TLSCertKey: string(c), resource.TLSKeyKey: string(k)}, nil
}
