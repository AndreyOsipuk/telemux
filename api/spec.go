// Package api - OpenAPI-спека telemux, вшитая в бинарь.
package api

import _ "embed"

// Spec - содержимое openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte
