// Package schema embeds the JSON Schemas that define the bundle contract.
package schema

import _ "embed"

//go:embed agent.v1.json
var AgentV1 []byte

//go:embed fixture.v1.json
var FixtureV1 []byte

//go:embed acceptance.v1.json
var AcceptanceV1 []byte

//go:embed check-fixture.v1.json
var CheckFixtureV1 []byte
