package git

import (
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

type Credentials struct {
	Username string
	Token    string
	RepoURL  string
	Branch   string
}

// Auth returns nil (a truly nil interface, not a typed-nil *http.BasicAuth)
// when no identity was resolved: go-git treats any non-nil AuthMethod as an
// auth attempt, which non-HTTP transports (git://) reject with "invalid auth
// method" and the HTTP transport would deref.
func (c *Credentials) Auth() transport.AuthMethod {
	if c.Username == "" && c.Token == "" {
		return nil
	}
	return &http.BasicAuth{
		Username: c.Username,
		Password: c.Token,
	}
}
