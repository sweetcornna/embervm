// Package lifecycle holds the EmberVM sandbox state machine. M1 implements
// the hot path only (RUNNING ⇄ PAUSED_HOT); the WARM/COLD/RECYCLED tiers
// from docs/zh/02 §3 arrive in M3 and their names are reserved here so the
// type is stable. Transitions are validated in exactly one place so every
// caller (node agent, control plane) agrees on what is legal.
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

	// Reserved for later milestones (M3 archive tiers); not yet reachable.
	StatePausedWarm   State = "PAUSED_WARM"
	StateArchivedCold State = "ARCHIVED_COLD"
	StateRecycled     State = "RECYCLED"
)

// transitions is the legal edge set. FAILED is reachable from any active
// (non-terminal) state and is added below rather than listed per-source.
var transitions = map[State]map[State]bool{
	StatePending:   {StateStarting: true},
	StateStarting:  {StateRunning: true},
	StateRunning:   {StatePausing: true, StateStopping: true},
	StatePausing:   {StatePausedHot: true},
	StatePausedHot: {StateResuming: true, StateStopping: true},
	StateResuming:  {StateRunning: true},
	StateStopping:  {StateStopped: true},
}

// activeStates may always fail.
var activeStates = map[State]bool{
	StateStarting: true, StateRunning: true, StatePausing: true,
	StatePausedHot: true, StateResuming: true, StateStopping: true,
}

// Terminal reports whether no transition leaves s.
func (s State) Terminal() bool {
	return s == StateStopped || s == StateFailed
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
