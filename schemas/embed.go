// Package schemas embeds the versioned JSON Schemas and reviewable data files.
package schemas

import _ "embed"

// CompanyV1 is the JSON Schema for company.yaml.
//
//go:embed company.v1.schema.json
var CompanyV1 []byte

// ModuleV1 is the JSON Schema for module.yaml.
//
//go:embed module.v1.schema.json
var ModuleV1 []byte

// AcceptedLicences is the default accepted-licence list (YAML).
//
//go:embed accepted-licences.yaml
var AcceptedLicences []byte

// Names lists known capability names (YAML).
//
//go:embed names.yaml
var Names []byte
