package mcp

import "encoding/json"

// jsonMarshal / jsonUnmarshal are tiny indirections so the rest of
// the package can stay free of the encoding/json import clutter.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
