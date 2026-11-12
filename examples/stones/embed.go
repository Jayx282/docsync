// Package stones embeds the example configuration and schema for `docsync init`.
package stones

import _ "embed"

// Config is the example docsync.yaml.
//
//go:embed docsync.yaml
var Config []byte

// Schema is the example SQL schema.
//
//go:embed schema.sql
var Schema []byte
