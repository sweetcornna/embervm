package netns

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func itoa(i int) string { return strconv.Itoa(i) }

type fakeRunner struct{ calls []string }

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return "", nil
}

func newTestPool(t *testing.T, size int) (*Pool, *fakeRunner) {
	t.Helper()
	f := &fakeRunner{}
	p := NewPool("/scripts", size)
	p.run = f.run
	return p, f
}

func TestPoolSetupCreatesAllNamespaces(t *testing.T) {
	p, f := newTestPool(t, 3)
	if err := p.Setup(context.Background()); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	for id := 0; id < 3; id++ {
		want := "/scripts/setup-network.sh --id " + itoa(id)
		if !contains(f.calls, want) {
			t.Errorf("missing setup call %q; calls=%v", want, f.calls)
		}
	}
}

func TestPoolAcquireReleaseCycle(t *testing.T) {
	p, _ := newTestPool(t, 2)
	if err := p.Setup(context.Background()); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	l1, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	l2, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	if l1.ID == l2.ID {
		t.Fatalf("Acquire returned duplicate id %d", l1.ID)
	}
	if l1.Netns != "ember"+itoa(l1.ID) || l1.GuestIP != "172.16.0.2" {
		t.Errorf("lease fields = %+v", l1)
	}
	if l1.NetnsPath != "/var/run/netns/ember"+itoa(l1.ID) {
		t.Errorf("NetnsPath = %q", l1.NetnsPath)
	}

	// Pool exhausted.
	if _, err := p.Acquire(); err == nil {
		t.Error("Acquire on empty pool: want error")
	}

	// Release makes the slot reusable.
	l1.Release()
	l3, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if l3.ID != l1.ID {
		t.Errorf("reacquired id = %d, want released %d", l3.ID, l1.ID)
	}
}

func TestPoolTeardownRemovesAll(t *testing.T) {
	p, f := newTestPool(t, 2)
	_ = p.Setup(context.Background())
	f.calls = nil
	if err := p.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for id := 0; id < 2; id++ {
		want := "/scripts/teardown-network.sh --id " + itoa(id)
		if !contains(f.calls, want) {
			t.Errorf("missing teardown call %q; calls=%v", want, f.calls)
		}
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	p, _ := newTestPool(t, 1)
	_ = p.Setup(context.Background())
	l, _ := p.Acquire()
	l.Release()
	l.Release() // double release must not create a duplicate free slot
	if _, err := p.Acquire(); err != nil {
		t.Fatalf("Acquire after double release: %v", err)
	}
	if _, err := p.Acquire(); err == nil {
		t.Error("second Acquire succeeded — double release leaked a slot")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
