package template

import (
	"bytes"
	"context"
	"testing"
)

func TestBuildInputValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		in   BuildInput
	}{
		{"neither source", BuildInput{GuestdPath: "g", OutPath: "o"}},
		{"both sources", BuildInput{Image: "alpine", TarSource: bytes.NewReader(nil), GuestdPath: "g", OutPath: "o"}},
		{"no guestd", BuildInput{Image: "alpine", OutPath: "o"}},
		{"no out", BuildInput{Image: "alpine", GuestdPath: "g"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(ctx, tc.in); err == nil {
				t.Errorf("Build(%+v) accepted invalid input", tc.in)
			}
		})
	}
}
