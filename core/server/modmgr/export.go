package modmgr

import (
	"fmt"

	"github.com/dodwmd/companyinacan/core/config"
)

// ParseCompany decodes company.yaml content without validating it against the schema.
func ParseCompany(data []byte) (*config.Company, error) {
	c, ps := decodeCompany("company.yaml", data)
	if c == nil {
		return nil, fmt.Errorf("company.yaml cannot be read: %s", firstProblem(ps))
	}
	return c, nil
}

// EnabledModules is the set of module ids company.yaml enables.
func EnabledModules(c *config.Company) map[string]bool { return enabledSet(c) }
