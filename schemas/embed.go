// Package schemas embeds the JSON Schemas so the CLI validates against the same files CI reads.
package schemas

import _ "embed"

// ToolV1 is the tool/v1 manifest schema.
//
//go:embed tool/v1/tool.schema.json
var ToolV1 []byte

// ToolV1ID is the $id of ToolV1.
const ToolV1ID = "https://github.com/moabualruz/rice-agent/schemas/tool/v1/tool.schema.json"

// ProviderV1 is the provider/v1 schema for registry/providers.json.
//
//go:embed provider/v1/provider.schema.json
var ProviderV1 []byte

// ProviderV1ID is the $id of ProviderV1.
const ProviderV1ID = "https://github.com/moabualruz/rice-agent/schemas/provider/v1/provider.schema.json"
