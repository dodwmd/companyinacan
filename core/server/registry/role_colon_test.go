package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func TestRegisterRefusesRoleNamesWithColon(t *testing.T) {
	r := newReg(t)
	err := r.Register(context.Background(), mod("evil", config.Module{Roles: map[string][]string{"forms:admin": {"evil.read"}}, Permissions: []string{"evil.read"}}))
	if !errors.Is(err, ErrReservedName) {
		t.Fatalf("a role name with ':' was accepted: %v", err)
	}
}
