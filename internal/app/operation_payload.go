package app

import "encoding/json"

// decodeOperationPayload extracts a typed operation intent or outcome
// payload from an Operation field that may hold either the exact Go value
// a caller stored (a handwritten fake keeping the value as constructed) or
// a JSON-decoded generic value (map[string]any, []any or a JSON scalar —
// what a real store's json.Unmarshal into `any` produces after a round
// trip through a JSON column). This never relies on Go type identity for a
// persisted payload: only a value already of type T is returned directly;
// anything else is re-marshaled to JSON and decoded into T, so a store
// that preserves the exact type and one that round-trips through JSON
// behave identically to callers. ok is false when payload is nil or does
// not decode into T.
func decodeOperationPayload[T any](payload any) (T, bool) {
	var zero T
	if payload == nil {
		return zero, false
	}
	if typed, ok := payload.(T); ok {
		return typed, true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return zero, false
	}
	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return zero, false
	}
	return decoded, true
}
