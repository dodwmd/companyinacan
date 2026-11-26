package modmgr

import (
	"context"
	"fmt"

	"github.com/dodwmd/companyinacan/core/config"
)

// ModuleExposure is what company.yaml says of a module's public access (specs/045-public-access).
type ModuleExposure struct {
	Domain  string
	Enabled bool // listed in company.yaml and not disabled
	Expose  config.Expose
}

// Exposure reads module id's exposure from the stored company.yaml. It is read-only: putting
// something on the internet is a change to the file.
func (s *Service) Exposure(ctx context.Context, id string) (ModuleExposure, error) {
	var x ModuleExposure
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return x, err
	}
	c, ps := decodeCompany("company.yaml", cur.Data)
	if c == nil {
		return x, fmt.Errorf("%w: %s", ErrInvalid, firstProblem(ps))
	}
	x.Domain = c.Domain
	if e, ok := c.Modules[id]; ok {
		x.Enabled, x.Expose = e.IsEnabled(), e.Exposure()
	}
	return x, nil
}
