package bossmcp

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Result rendering shared by every hosted-only tool family (triggers, session
// webhooks). It lives apart from any one family so a change made for one
// family is visibly a change to all of them.
//
// Results are rendered with protojson (proto field names, enum value names),
// not encoding/json over the generated structs, because the inputs take enum
// value names: an agent reads back exactly the spelling it writes.

// hostedSecretNote accompanies a one-time signing secret in a hosted tool
// result. The secret can never be read back, only rotated.
const hostedSecretNote = "store it now; it is not shown again"

// hostedJSON renders hosted-tier messages for tool results. EmitDefaultValues
// keeps false/zero scalars visible (is_enabled: false is information) without
// emitting unset messages such as an absent last_invocation.
var hostedJSON = protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}

func hostedMessageJSON(m proto.Message) (json.RawMessage, error) {
	b, err := hostedJSON.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal %T: %w", m, err)
	}
	return b, nil
}

// hostedMessagesJSON renders a list, as [] rather than null when empty.
func hostedMessagesJSON[M proto.Message](msgs []M) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		b, err := hostedMessageJSON(m)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// hostedResult is jsonResult for a value built from hostedMessageJSON parts.
func hostedResult(v any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	r, err := jsonResult(v)
	return r, nil, err
}
