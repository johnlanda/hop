package run

// transitionTable is the exhaustive set of valid (from, to) state pairs for
// one entity's state machine, keyed by the section 5 state tables of
// docs/plan/phase-2-design.md. A pair the table does not list is invalid.
type transitionTable[S comparable] map[S]map[S]bool

// newTransitionTable builds a transitionTable from a flat list of (from, to)
// pairs.
func newTransitionTable[S comparable](pairs [][2]S) transitionTable[S] {
	table := make(transitionTable[S], len(pairs))
	for _, pair := range pairs {
		from, to := pair[0], pair[1]
		targets, ok := table[from]
		if !ok {
			targets = make(map[S]bool, 1)
			table[from] = targets
		}
		targets[to] = true
	}
	return table
}

// valid reports whether moving from from to to is a listed transition.
func (t transitionTable[S]) valid(from, to S) bool {
	return t[from][to]
}

// fromAny returns the (from, to) pairs pairing every state in from with to,
// a compact way to express one table row whose cause names several source
// states for the same target.
func fromAny[S comparable](to S, from ...S) [][2]S {
	pairs := make([][2]S, len(from))
	for i, f := range from {
		pairs[i] = [2]S{f, to}
	}
	return pairs
}
