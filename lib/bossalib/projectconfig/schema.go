package projectconfig

import (
	"bytes"
	_ "embed"
)

// SchemaPath is where the JSON Schema lives in the source tree, for editors.
const SchemaPath = "lib/bossalib/projectconfig/schema/boss-project.schema.json"

//go:embed schema/boss-project.schema.json
var schemaJSON []byte

// Schema returns the embedded draft 2020-12 JSON Schema for the file. It is
// for editors; boss itself decodes with Decode and never validates against it.
func Schema() []byte { return bytes.Clone(schemaJSON) }
