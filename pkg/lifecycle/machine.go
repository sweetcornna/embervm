// Package lifecycle holds the EmberVM sandbox state machine: the M1 hot
// path (RUNNING ⇄ PAUSED_HOT) plus the M3 archive tiers of docs/zh/02 §3
// (PAUSED_HOT → PAUSED_WARM → ARCHIVED_COLD → RECYCLED on TTLs, resume legal
// from every tier except RECYCLED). Transitions are validated in exactly one
// place so every caller (node agent, control plane) agrees on what is legal.
package lifecycle

import "fmt"

// State is a sandbox lifecycle state.
type State string

const (
	StatePending   State = "PENDING"
	StateStarting  State = "STARTING"
	StateRunning   State = "RUNNING"
	StatePausing   State = "PAUSING"
	StatePausedHot State = "PAUSED_HOT"
	StateResuming  State = "RESUMING"
	StateStopping  State = "STOPPING"
	StateStopped   State = "STOPPED"
	StateFailed    State = "FAILED"

	// M3 archive tiers (docs/zh/02 §3): WARM = L1 only (node released),
	// COLD = cold store only (synthetic full), RECYCLED = artifacts only.
	StatePausedWarm   State = "PAUSED_WARM"
	StateArchivedCold State = "ARCHIVED_COLD"
	StateRecycled     State = "RECYCLED"
)

// transitions is the legal edge set. FAILED is reachable from any active
// (non-terminal) state and is added below rather than listed per-source.
var transitions = map[State]map[State]bool{
	StatePending:      {StateStarting: true},
	StateStarting:     {StateRunning: true},
	StateRunning:      {StatePausing: true, StateStopping: true},
	StatePausing:      {StatePausedHot: true},
	StatePausedHot:    {StateResuming: true, StateStopping: true, StatePausedWarm: true},
	StatePausedWarm:   {StateResuming: true, StateArchivedCold: true, StateStopping: true},
	StateArchivedCold: {StateResuming: true, StateRecycled: true, StateStopping: true},
	StateRecycled:     {StateStopping: true},
	StateResuming:     {StateRunning: true},
	StateStopping:     {StateStopped: true},
	// A FAILED sandbox may attempt recovery from its last write-through
	// snapshot (M4: node death marks its actives FAILED; resume re-places
	// them) or be stopped. A restore that fails lands back in FAILED.
	StateFailed: {StateResuming: true, StateStopping: true},
}

// activeStates may always fail.
var activeStates = map[State]bool{
	StateStarting: true, StateRunning: true, StatePausing: true,
	StatePausedHot: true, StatePausedWarm: true, StateArchivedCold: true,
	StateResuming: true, StateStopping: true,
}

// Terminal reports whether no transition leaves s.
func (s State) Terminal() bool {
	return s == StateStopped
}

// Paused reports whether s is a paused/archived tier that a TTL transition
// or a resume can act on.
func Paused(s State) bool {
	return s == StatePausedHot || s == StatePausedWarm || s == StateArchivedCold
}

// Validate reports whether from→to is a legal transition.
func Validate(from, to State) error {
	if to == StateFailed && activeStates[from] {
		return nil
	}
	if transitions[from][to] {
		return nil
	}
	return fmt.Errorf("illegal lifecycle transition %s -> %s", from, to)
}

// Machine tracks one sandbox's current state.
type Machine struct {
	state State
}

// New starts a machine in the given state.
func New(initial State) *Machine { return &Machine{state: initial} }

// State returns the current state.
func (m *Machine) State() State { return m.state }

// To validates and applies a transition, leaving the state unchanged on
// error.
func (m *Machine) To(to State) error {
	if err := Validate(m.state, to); err != nil {
		return err
	}
	m.state = to
	return nil
}
