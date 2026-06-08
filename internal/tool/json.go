package tool

import "encoding/json"

// jsonMarshal is a tiny indirection so we can swap encoders (e.g. sonic)
// in one place if size ever becomes a concern. For now stdlib is plenty.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
