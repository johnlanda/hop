package app

import "testing"

// checkRunIntentLike mirrors CheckRunIntent's shape for this internal test,
// so the test lives beside decodeOperationPayload without depending on
// another file's exported type.
type checkRunIntentLike struct {
	CheckoutPath string   `json:"checkout_path"`
	CheckArgv    []string `json:"check_argv"`
}

// TestDecodeOperationPayload proves decodeOperationPayload never relies on
// Go type identity for a persisted Operation field: it accepts both the
// exact typed value a handwritten fake keeps and the generic
// map[string]any shape a real store's json.Unmarshal into `any` produces
// after a round trip through a JSON column (internal/adapters/sqlite).
func TestDecodeOperationPayload(t *testing.T) {
	t.Run("already the exact typed value", func(t *testing.T) {
		want := checkRunIntentLike{CheckoutPath: "/checkouts/1", CheckArgv: []string{"sh", "check.sh"}}
		got, ok := decodeOperationPayload[checkRunIntentLike](want)
		if !ok {
			t.Fatalf("decodeOperationPayload() ok = false, want true")
		}
		if got.CheckoutPath != want.CheckoutPath || len(got.CheckArgv) != len(want.CheckArgv) {
			t.Fatalf("decodeOperationPayload() = %+v, want %+v", got, want)
		}
	})

	t.Run("JSON-decoded map[string]any, as a real store returns", func(t *testing.T) {
		// The shape encoding/json produces unmarshaling a JSON object into
		// `any`: map[string]any with the field's own json tags as keys.
		persisted := any(map[string]any{
			"checkout_path": "/checkouts/2",
			"check_argv":    []any{"sh", "check.sh"},
		})
		got, ok := decodeOperationPayload[checkRunIntentLike](persisted)
		if !ok {
			t.Fatalf("decodeOperationPayload() ok = false, want true")
		}
		if got.CheckoutPath != "/checkouts/2" {
			t.Fatalf("CheckoutPath = %q, want %q", got.CheckoutPath, "/checkouts/2")
		}
		if len(got.CheckArgv) != 2 || got.CheckArgv[0] != "sh" || got.CheckArgv[1] != "check.sh" {
			t.Fatalf("CheckArgv = %v, want [sh check.sh]", got.CheckArgv)
		}
	})

	t.Run("nil payload decodes to nothing", func(t *testing.T) {
		if _, ok := decodeOperationPayload[checkRunIntentLike](nil); ok {
			t.Fatalf("decodeOperationPayload(nil) ok = true, want false")
		}
	})

	t.Run("a value of an unrelated shape fails rather than panicking", func(t *testing.T) {
		if _, ok := decodeOperationPayload[checkRunIntentLike]("not an object"); ok {
			t.Fatalf("decodeOperationPayload() ok = true for an incompatible payload, want false")
		}
	})
}
