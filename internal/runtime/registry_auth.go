package runtime

// RegistryAuth is the login for one registry host. The server sends them for
// the image pulls of a replica (the template's image_pull_secrets, K-19).
type RegistryAuth struct {
	Host     string
	Username string
	Password string
}

// String omits the password, so a logged RegistryAuth cannot leak it.
func (a RegistryAuth) String() string {
	return a.Host + " (user " + a.Username + ")"
}

// Credentials returns the login for host. Both strings are empty when auth has
// none for host, and the pull is then anonymous.
func Credentials(auth []RegistryAuth, host string) (username, password string) {
	for _, a := range auth {
		if a.Host == host {
			return a.Username, a.Password
		}
	}
	return "", ""
}
