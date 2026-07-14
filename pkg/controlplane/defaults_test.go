package controlplane

import (
	"encoding/json"
	"testing"
)

// TestResolveGeometry walks the M7 decision table (ADR-0008).
func TestResolveGeometry(t *testing.T) {
	on, off := boolPtr(true), boolPtr(false)
	d := ElasticDefaults{} // platform defaults: 1/256 base, 4/4096 ceiling, disk 15

	cases := []struct {
		name string
		in   createSandboxBody
		want createSandboxBody // zero Autoscale pointer = "expect nil"
		err  bool
	}{
		{
			name: "no geometry: default elastic, autoscale on",
			in:   createSandboxBody{},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 15, MaxVCPUs: 4, MaxMemoryMiB: 4096, Autoscale: on},
		},
		{
			name: "no geometry, autoscale:false: elastic geometry, engine off",
			in:   createSandboxBody{Autoscale: off},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 15, MaxVCPUs: 4, MaxMemoryMiB: 4096, Autoscale: off},
		},
		{
			name: "no geometry, autoscale:true: same as nil",
			in:   createSandboxBody{Autoscale: on},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 15, MaxVCPUs: 4, MaxMemoryMiB: 4096, Autoscale: on},
		},
		{
			name: "ceiling only: default base, custom ceiling, autoscale on",
			in:   createSandboxBody{MaxMemoryMiB: 8192},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 15, MaxMemoryMiB: 8192, Autoscale: on},
		},
		{
			name: "vcpu ceiling only: memory stays fixed",
			in:   createSandboxBody{MaxVCPUs: 2},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 15, MaxVCPUs: 2, Autoscale: on},
		},
		{
			name: "explicit base, no max: fixed geometry (M6 contract)",
			in:   createSandboxBody{VCPUs: 2, MemoryMiB: 512},
			want: createSandboxBody{VCPUs: 2, MemoryMiB: 512, DataDiskGiB: 15},
		},
		{
			name: "partial base fills the missing half",
			in:   createSandboxBody{VCPUs: 2},
			want: createSandboxBody{VCPUs: 2, MemoryMiB: 256, DataDiskGiB: 15},
		},
		{
			name: "explicit base + autoscale:true, no max: rejected",
			in:   createSandboxBody{MemoryMiB: 512, Autoscale: on},
			err:  true,
		},
		{
			name: "explicit base + max: untouched (user-declared elastic)",
			in:   createSandboxBody{VCPUs: 1, MemoryMiB: 512, MaxMemoryMiB: 1024, Autoscale: on},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 512, DataDiskGiB: 15, MaxMemoryMiB: 1024, Autoscale: on},
		},
		{
			name: "explicit base + max, autoscale omitted: engine stays off",
			in:   createSandboxBody{VCPUs: 1, MemoryMiB: 512, MaxMemoryMiB: 1024},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 512, DataDiskGiB: 15, MaxMemoryMiB: 1024},
		},
		{
			name: "explicit disk is kept",
			in:   createSandboxBody{DataDiskGiB: 3},
			want: createSandboxBody{VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 3, MaxVCPUs: 4, MaxMemoryMiB: 4096, Autoscale: on},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveGeometry(tc.in, d)
			if tc.err {
				if err == nil {
					t.Fatalf("resolveGeometry(%+v) = %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveGeometry(%+v): %v", tc.in, err)
			}
			if got.VCPUs != tc.want.VCPUs || got.MemoryMiB != tc.want.MemoryMiB ||
				got.DataDiskGiB != tc.want.DataDiskGiB ||
				got.MaxVCPUs != tc.want.MaxVCPUs || got.MaxMemoryMiB != tc.want.MaxMemoryMiB {
				t.Errorf("geometry = %d/%dMiB disk %d max %d/%dMiB, want %d/%dMiB disk %d max %d/%dMiB",
					got.VCPUs, got.MemoryMiB, got.DataDiskGiB, got.MaxVCPUs, got.MaxMemoryMiB,
					tc.want.VCPUs, tc.want.MemoryMiB, tc.want.DataDiskGiB, tc.want.MaxVCPUs, tc.want.MaxMemoryMiB)
			}
			gotAS := got.Autoscale != nil && *got.Autoscale
			wantAS := tc.want.Autoscale != nil && *tc.want.Autoscale
			if gotAS != wantAS {
				t.Errorf("autoscale = %v, want %v", gotAS, wantAS)
			}
		})
	}
}

func TestResolveGeometryDisabled(t *testing.T) {
	in := createSandboxBody{Autoscale: boolPtr(true)}
	got, err := resolveGeometry(in, ElasticDefaults{Disabled: true})
	if err != nil {
		t.Fatalf("resolveGeometry: %v", err)
	}
	// Byte-for-byte pass-through: zeros reach the node, exactly pre-M7.
	if got.VCPUs != 0 || got.MemoryMiB != 0 || got.MaxMemoryMiB != 0 || got.MaxVCPUs != 0 || got.DataDiskGiB != 0 {
		t.Errorf("Disabled mutated the body: %+v", got)
	}
}

// TestResolveGeometryCustomDefaults checks the ceiling knobs flow through
// and a below-base ceiling is clamped rather than producing an invalid pair.
func TestResolveGeometryCustomDefaults(t *testing.T) {
	d := ElasticDefaults{MaxMemoryMiB: 2048, MaxVCPUs: 2}
	got, err := resolveGeometry(createSandboxBody{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxMemoryMiB != 2048 || got.MaxVCPUs != 2 {
		t.Errorf("ceiling = %d/%d, want 2048/2", got.MaxMemoryMiB, got.MaxVCPUs)
	}

	d = ElasticDefaults{MaxMemoryMiB: 64} // below the 256 base
	got, err = resolveGeometry(createSandboxBody{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxMemoryMiB != got.MemoryMiB {
		t.Errorf("below-base ceiling: max = %d, want clamp to base %d", got.MaxMemoryMiB, got.MemoryMiB)
	}
}

func TestElasticDefaultsFromEnv(t *testing.T) {
	t.Setenv("EMBERVM_DEFAULT_ELASTIC", "false")
	t.Setenv("EMBERVM_DEFAULT_MAX_MEMORY_MIB", "1000") // not slot-aligned
	t.Setenv("EMBERVM_DEFAULT_MAX_VCPUS", "8")
	d, err := ElasticDefaultsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Disabled {
		t.Error("EMBERVM_DEFAULT_ELASTIC=false should disable")
	}
	// 256 + roundUpToSlot(1000-256) = 256 + 768 = 1024.
	if d.MaxMemoryMiB != 1024 {
		t.Errorf("MaxMemoryMiB = %d, want pre-rounded 1024", d.MaxMemoryMiB)
	}
	if d.MaxVCPUs != 8 {
		t.Errorf("MaxVCPUs = %d, want 8", d.MaxVCPUs)
	}

	t.Setenv("EMBERVM_DEFAULT_MAX_MEMORY_MIB", "1")
	if _, err := ElasticDefaultsFromEnv(); err == nil {
		t.Error("ceiling below the default base should error")
	}
	t.Setenv("EMBERVM_DEFAULT_MAX_MEMORY_MIB", "")
	t.Setenv("EMBERVM_DEFAULT_MAX_VCPUS", "99")
	if _, err := ElasticDefaultsFromEnv(); err == nil {
		t.Error("vcpu ceiling above 64 should error")
	}
}

// TestCreateBodyAutoscaleJSON pins the tri-state wire behavior the *bool
// exists for: omitted vs false must be distinguishable.
func TestCreateBodyAutoscaleJSON(t *testing.T) {
	var omitted, explicit createSandboxBody
	if err := json.Unmarshal([]byte(`{"template_id":"t"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.Autoscale != nil {
		t.Error("omitted autoscale should unmarshal to nil")
	}
	if err := json.Unmarshal([]byte(`{"template_id":"t","autoscale":false}`), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.Autoscale == nil || *explicit.Autoscale {
		t.Error("autoscale:false should unmarshal to a false pointer")
	}
}
