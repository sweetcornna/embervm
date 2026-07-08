package lifecycle

import "testing"

func TestLegalTransitions(t *testing.T) {
	legal := []struct{ from, to State }{
		{StatePending, StateStarting},
		{StateStarting, StateRunning},
		{StateRunning, StatePausing},
		{StatePausing, StatePausedHot},
		{StatePausedHot, StateResuming},
		{StateResuming, StateRunning},
		{StateRunning, StateStopping},
		{StatePausedHot, StateStopping},
		{StateStopping, StateStopped},
		{StateStarting, StateFailed},
		{StateRunning, StateFailed},
		{StatePausing, StateFailed},
		{StateResuming, StateFailed},
	}
	for _, tc := range legal {
		if err := Validate(tc.from, tc.to); err != nil {
			t.Errorf("Validate(%s,%s) = %v, want nil", tc.from, tc.to, err)
		}
	}
}

func TestIllegalTransitions(t *testing.T) {
	illegal := []struct{ from, to State }{
		{StateStopped, StateRunning},   // terminal
		{StatePending, StateRunning},   // must pass through STARTING
		{StateRunning, StatePausedHot}, // must pass through PAUSING
		{StatePausedHot, StateRunning}, // must pass through RESUMING
		{StateFailed, StateRunning},    // terminal
		{StateRunning, StateRunning},   // no self-loop
	}
	for _, tc := range illegal {
		if err := Validate(tc.from, tc.to); err == nil {
			t.Errorf("Validate(%s,%s) = nil, want error", tc.from, tc.to)
		}
	}
}

func TestMachineTransition(t *testing.T) {
	m := New(StatePending)
	for _, to := range []State{StateStarting, StateRunning, StatePausing, StatePausedHot, StateResuming, StateRunning} {
		if err := m.To(to); err != nil {
			t.Fatalf("To(%s): %v", to, err)
		}
	}
	if m.State() != StateRunning {
		t.Errorf("final state = %s, want RUNNING", m.State())
	}
	// An illegal transition is rejected and leaves the state unchanged.
	if err := m.To(StatePausedHot); err == nil {
		t.Error("To(PAUSED_HOT) from RUNNING: want error")
	}
	if m.State() != StateRunning {
		t.Errorf("state after rejected transition = %s, want RUNNING", m.State())
	}
}

func TestTerminalStates(t *testing.T) {
	if !StateStopped.Terminal() {
		t.Error("STOPPED must be terminal")
	}
	// Since M4, FAILED is recoverable: a node death marks its actives
	// FAILED and a resume re-places them from the last snapshot.
	if StateFailed.Terminal() {
		t.Error("FAILED must be recoverable (resume from last snapshot)")
	}
	if err := Validate(StateFailed, StateResuming); err != nil {
		t.Errorf("FAILED -> RESUMING must be legal: %v", err)
	}
	if StateRunning.Terminal() {
		t.Error("RUNNING must not be terminal")
	}
}

func TestMachineCAS(t *testing.T) {
	m := New(StateRunning)
	// CAS loses when the observed state is stale — even though the target
	// would be a legal transition from the CURRENT state (FAILED is legal
	// from any active state, which is exactly why the watchdog cannot use
	// a plain To: a sandbox that moved RUNNING -> PAUSING mid-scan must
	// not be reaped).
	if err := m.CAS(StatePausedHot, StateFailed); err == nil {
		t.Error("CAS with stale from-state: want error")
	}
	if m.State() != StateRunning {
		t.Errorf("state after lost CAS = %s, want RUNNING", m.State())
	}
	if err := m.CAS(StateRunning, StateFailed); err != nil {
		t.Fatalf("CAS(RUNNING, FAILED): %v", err)
	}
	if m.State() != StateFailed {
		t.Errorf("state after won CAS = %s, want FAILED", m.State())
	}
	// A CAS to an illegal target is rejected even with the right from.
	if err := m.CAS(StateFailed, StateRunning); err == nil {
		t.Error("CAS(FAILED, RUNNING): want error (illegal edge)")
	}
}
