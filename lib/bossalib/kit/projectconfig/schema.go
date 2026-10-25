package projectconfig

import (
	"bytes"
	_ "embed"
)

// SchemaPath is where the JSON Schema lives in the source tree, for editors.
// The file is generated from the Go types in this package by TestSchemaGenerated
// (`make projectconfig-schema`); never edit it by hand.
const SchemaPath = "lib/bossalib/kit/projectconfig/schema/boss-project.schema.json"

//go:embed schema/boss-project.schema.json
var schemaJSON []byte

// Schema returns the embedded draft 2020-12 JSON Schema for the file. It is
// for editors; boss itself decodes with Decode and never validates against it.
func Schema() []byte { return bytes.Clone(schemaJSON) }
