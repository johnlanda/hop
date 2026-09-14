package herdr_test

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// barrierTimeout bounds a wait for a goroutine to reach or leave a state.
const barrierTimeout = 5 * time.Second

// stackDump returns every goroutine's stack.
func stackDump() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

// goroutineParkedInSelect reports whether some goroutine is currently parked
// in a select ("[select]:") somewhere below the named symbol. This is the
// precise "blocked in its send select" state, not merely that the symbol
// appears on a stack.
func goroutineParkedInSelect(symbol string) bool {
	for _, g := range strings.Split(stackDump(), "\n\n") {
		if strings.Contains(g, "[select]:") && strings.Contains(g, symbol) {
			return true
		}
	}
	return false
}

// stackContains reports whether the named symbol appears in any goroutine
// stack, i.e. that goroutine has not yet returned.
func stackContains(symbol string) bool {
	return strings.Contains(stackDump(), symbol)
}

// waitParkedInSelect blocks until a goroutine is parked in a select under the
// symbol, so a test can cancel or close exactly when the pump is stuck in its
// send rather than racing it.
func waitParkedInSelect(t *testing.T, symbol string) {
	t.Helper()
	deadline := time.Now().Add(barrierTimeout)
	for !goroutineParkedInSelect(symbol) {
		if time.Now().After(deadline) {
			t.Fatalf("no goroutine parked in a select under %s", symbol)
		}
		runtime.Gosched()
	}
}

// assertGoroutineGone fails unless the named symbol leaves every goroutine
// stack within the timeout: the goroutine returned rather than leaking. It
// does not read the pump's channel, so it cannot itself release a blocked
// send — the whole point of the check.
func assertGoroutineGone(t *testing.T, symbol string) {
	t.Helper()
	deadline := time.Now().Add(barrierTimeout)
	for stackContains(symbol) {
		if time.Now().After(deadline) {
			t.Fatalf("%s is still running; the pump goroutine leaked", symbol)
		}
		runtime.Gosched()
	}
}
