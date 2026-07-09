package lifecycle

import "testing"

// TestAbortedPauseRollsBackToRunning pins the PAUSING→RUNNING edge: a pause
// that fails before the snapshot resumes Firecracker in place, so the
// machine must be able to return to RUNNING instead of wedging (PAUSING's
// only other forward edge is →PAUSED_HOT, which a failed pause never takes).
func TestAbortedPauseRollsBackToRunning(t *testing.T) {
	m := New(StateRunning)
	if err := m.To(StatePausing); err != nil {
		t.Fatal(err)
	}
	if err := m.CAS(StatePausing, StateRunning); err != nil {
		t.Fatalf("aborted pause rollback: %v", err)
	}
	// And the sandbox is fully usable again.
	if err := m.To(StatePausing); err != nil {
		t.Fatalf("re-pause after aborted pause: %v", err)
	}
	if err := m.To(StatePausedHot); err != nil {
		t.Fatal(err)
	}
}
