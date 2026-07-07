package uffd

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUnmarshalMapping(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want GuestRegionUffdMapping
	}{
		{
			name: "v1.16 field names",
			in:   `{"base_host_virt_addr":140000000000,"size":268435456,"offset":0,"page_size":4096}`,
			want: GuestRegionUffdMapping{140000000000, 268435456, 0, 4096},
		},
		{
			name: "legacy page_size_kib alias (value in bytes despite the name)",
			in:   `{"base_host_virt_addr":1,"size":2,"offset":3,"page_size_kib":4096}`,
			want: GuestRegionUffdMapping{1, 2, 3, 4096},
		},
		{
			name: "no page size at all defaults to 4096",
			in:   `{"base_host_virt_addr":1,"size":2,"offset":3}`,
			want: GuestRegionUffdMapping{1, 2, 3, 4096},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got GuestRegionUffdMapping
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMappingValidate(t *testing.T) {
	bad := []GuestRegionUffdMapping{
		{BaseHostVirtAddr: 0, Size: 0, PageSize: 4096},      // zero size
		{BaseHostVirtAddr: 0, Size: 4096, PageSize: 3000},   // not a power of two
		{BaseHostVirtAddr: 123, Size: 4096, PageSize: 4096}, // misaligned base
	}
	for i, m := range bad {
		if err := m.validate(); err == nil {
			t.Errorf("case %d: expected validation error for %+v", i, m)
		}
	}
	good := GuestRegionUffdMapping{BaseHostVirtAddr: 1 << 30, Size: 4096, PageSize: 4096}
	if err := good.validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// shortSockPath returns a unix socket path short enough for every platform:
// macOS caps sun_path at 104 bytes and t.TempDir() embeds the full test name,
// which can blow past that.
func shortSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "uffd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "uffd.sock")
}

// fakeFirecracker connects to the handler socket and performs the v1.16-style
// handshake: mappings JSON plus an fd via SCM_RIGHTS, in a single message.
func fakeFirecracker(t *testing.T, sock string, payload []byte, split bool) {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Errorf("dial: %v", err)
		return
	}
	defer conn.Close()

	// Any fd works as a stand-in for the userfaultfd.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Errorf("open devnull: %v", err)
		return
	}
	defer f.Close()
	rights := unix.UnixRights(int(f.Fd()))

	if split {
		half := len(payload) / 2
		if _, _, err := conn.WriteMsgUnix(payload[:half], rights, nil); err != nil {
			t.Errorf("write 1: %v", err)
			return
		}
		time.Sleep(10 * time.Millisecond)
		if _, err := conn.Write(payload[half:]); err != nil {
			t.Errorf("write 2: %v", err)
			return
		}
	} else {
		if _, _, err := conn.WriteMsgUnix(payload, rights, nil); err != nil {
			t.Errorf("write: %v", err)
			return
		}
	}
	// Keep the peer open long enough for the handshake to complete.
	time.Sleep(200 * time.Millisecond)
}

func TestHandshake(t *testing.T) {
	mappings := []GuestRegionUffdMapping{
		{BaseHostVirtAddr: 0x7f0000000000, Size: 1 << 28, Offset: 0, PageSize: 4096},
		{BaseHostVirtAddr: 0x7f8000000000, Size: 1 << 20, Offset: 1 << 28, PageSize: 4096},
	}
	payload, err := json.Marshal(mappings)
	if err != nil {
		t.Fatal(err)
	}

	for _, split := range []bool{false, true} {
		name := "single-message"
		if split {
			name = "fragmented-delivery"
		}
		t.Run(name, func(t *testing.T) {
			sock := shortSockPath(t)
			l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()

			go fakeFirecracker(t, sock, payload, split)

			got, fd, conn, err := Handshake(l, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			defer unix.Close(fd)

			if len(got) != len(mappings) {
				t.Fatalf("got %d mappings, want %d", len(got), len(mappings))
			}
			for i := range got {
				if got[i] != mappings[i] {
					t.Errorf("mapping %d: got %+v, want %+v", i, got[i], mappings[i])
				}
			}
			// The received fd must be a real, usable descriptor.
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				t.Errorf("received fd unusable: %v", err)
			}
		})
	}
}

func TestHandshakeTimeout(t *testing.T) {
	sock := shortSockPath(t)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, _, _, err := Handshake(l, 50*time.Millisecond); err == nil {
		t.Fatal("expected timeout error")
	}
}
