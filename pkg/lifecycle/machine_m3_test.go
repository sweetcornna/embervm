package lifecycle

import "testing"

// The M3 tier chain and its resume edges.
func TestTierTransitions(t *testing.T) {
	legal := [][2]State{
		{StatePausedHot, StatePausedWarm},
		{StatePausedWarm, StateArchivedCold},
		{StateArchivedCold, StateRecycled},
		{StatePausedWarm, StateResuming},
		{StateArchivedCold, StateResuming},
		{StatePausedWarm, StateStopping},
		{StateArchivedCold, StateStopping},
		{StateRecycled, StateStopping},
		{StatePausedWarm, StateFailed},
		{StateArchivedCold, StateFailed},
	}
	for _, e := range legal {
		if err := Validate(e[0], e[1]); err != nil {
			t.Errorf("Validate(%s -> %s) = %v, want legal", e[0], e[1], err)
		}
	}
	illegal := [][2]State{
		{StateRecycled, StateResuming},      // RECYCLED never resumes in place
		{StatePausedHot, StateArchivedCold}, // tiers may not be skipped
		{StatePausedWarm, StatePausedHot},   // no promotion without a resume
		{StateRecycled, StateFailed},        // recycled is passive, cannot fail
	}
	for _, e := range illegal {
		if err := Validate(e[0], e[1]); err == nil {
			t.Errorf("Validate(%s -> %s) accepted, want illegal", e[0], e[1])
		}
	}
}

func TestPausedPredicate(t *testing.T) {
	for _, s := range []State{StatePausedHot, StatePausedWarm, StateArchivedCold} {
		if !Paused(s) {
			t.Errorf("Paused(%s) = false", s)
		}
	}
	for _, s := range []State{StateRunning, StateRecycled, StateStopped, StateResuming} {
		if Paused(s) {
			t.Errorf("Paused(%s) = true", s)
		}
	}
}
