package provision

import "context"

// Stub is a documented placeholder behind the Connector interface for a SaaS the module does not
// talk to yet (google-workspace, github, slack). It makes NO network call and every operation
// fails permanently with the code "unimplemented", so a workflow that names a stub dead-letters
// loudly instead of silently pretending that access was granted or removed. UNVERIFIED: no part
// of these adapters has ever run against the real service. docs/provisioning.md lists what each
// would need (OAuth scopes, the admin API calls) before it can be implemented.
type Stub struct {
	ConnName string
	Kind     string
}

// Name implements Connector.
func (s *Stub) Name() string { return s.ConnName }

// Verified implements Connector.
func (s *Stub) Verified() bool { return false }

// CreateAccount implements Connector.
func (s *Stub) CreateAccount(context.Context, string, Identity) (string, error) {
	return "", permanent("unimplemented")
}

// UpdateAccount implements Connector.
func (s *Stub) UpdateAccount(context.Context, string, string, Identity) error {
	return permanent("unimplemented")
}

// DisableAccount implements Connector.
func (s *Stub) DisableAccount(context.Context, string, string) error {
	return permanent("unimplemented")
}

// DeleteAccount implements Connector.
func (s *Stub) DeleteAccount(context.Context, string, string) error {
	return permanent("unimplemented")
}

// AddToGroup implements Connector.
func (s *Stub) AddToGroup(context.Context, string, string, string) error {
	return permanent("unimplemented")
}

// RemoveFromGroup implements Connector.
func (s *Stub) RemoveFromGroup(context.Context, string, string, string) error {
	return permanent("unimplemented")
}

// StubKinds are the connector types that are stubs.
var StubKinds = []string{"google-workspace", "github", "slack"}
