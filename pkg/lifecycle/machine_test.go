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
	if !StateStopped.Terminal() || !StateFailed.Terminal() {
		t.Error("STOPPED and FAILED must be terminal")
	}
	if StateRunning.Terminal() {
		t.Error("RUNNING must not be terminal")
	}
}
