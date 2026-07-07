package uffd

import (
	"encoding/json"
	"os"
	"sync/atomic"
)

// Stats counts what the handler did. Counters are updated from both the
// fault-serving loop and the prefetch goroutine, hence atomics.
type Stats struct {
	HandshakeUnixNs     atomic.Int64
	FirstFaultUnixNs    atomic.Int64
	FaultsServed        atomic.Uint64
	BytesCopiedFault    atomic.Uint64
	BytesCopiedPrefetch atomic.Uint64
	Zeropages           atomic.Uint64
	RemoveEvents        atomic.Uint64
	Regions             atomic.Int64
	MemTotalBytes       atomic.Uint64
}

// StatsSnapshot is the plain, JSON-serializable view of Stats. Field names
// are the contract consumed by fc-restore.sh and genreport.
type StatsSnapshot struct {
	HandshakeUnixNs     int64  `json:"handshake_unix_ns"`
	FirstFaultUnixNs    int64  `json:"first_fault_unix_ns"`
	FaultsServed        uint64 `json:"faults_served"`
	BytesCopiedFault    uint64 `json:"bytes_copied_fault"`
	BytesCopiedPrefetch uint64 `json:"bytes_copied_prefetch"`
	Zeropages           uint64 `json:"zeropages"`
	RemoveEvents        uint64 `json:"remove_events"`
	Regions             int64  `json:"regions"`
	MemTotalBytes       uint64 `json:"mem_total_bytes"`
}

func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		HandshakeUnixNs:     s.HandshakeUnixNs.Load(),
		FirstFaultUnixNs:    s.FirstFaultUnixNs.Load(),
		FaultsServed:        s.FaultsServed.Load(),
		BytesCopiedFault:    s.BytesCopiedFault.Load(),
		BytesCopiedPrefetch: s.BytesCopiedPrefetch.Load(),
		Zeropages:           s.Zeropages.Load(),
		RemoveEvents:        s.RemoveEvents.Load(),
		Regions:             s.Regions.Load(),
		MemTotalBytes:       s.MemTotalBytes.Load(),
	}
}

// WriteFile writes the snapshot as indented JSON. An empty path is a no-op.
func (s *Stats) WriteFile(path string) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
