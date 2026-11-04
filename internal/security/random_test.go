package security

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

type unavailableEntropy struct{ err error }

func (reader unavailableEntropy) Read([]byte) (int, error) { return 0, reader.err }

func TestRandomIntRangePreservesEntropyErrors(t *testing.T) {
	previous := rand.Reader
	defer func() { rand.Reader = previous }()
	entropyErr := errors.New("entropy unavailable")
	rand.Reader = unavailableEntropy{err: entropyErr}
	if _, err := RandomIntRange(1, 100); !errors.Is(err, entropyErr) {
		t.Fatalf("random failure was hidden: %v", err)
	}
	if got, err := RandomIntRange(7, 7); err != nil || got != 7 {
		t.Fatalf("fixed range needs no entropy: %d %v", got, err)
	}
	if _, err := RandomIntRange(7, 1); err == nil {
		t.Fatal("reversed range accepted")
	}
}

func TestRandomIntRangeSupportsIntegerBoundaries(t *testing.T) {
	previous := rand.Reader
	defer func() { rand.Reader = previous }()
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	for _, tc := range []struct {
		name           string
		min, max, want int
		entropy        byte
	}{
		{"minimum", minInt, maxInt, minInt, 0},
		{"maximum", minInt, maxInt, maxInt, 255},
		{"negative", -10, -1, -10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rand.Reader = bytes.NewReader(bytes.Repeat([]byte{tc.entropy}, 8))
			if got, err := RandomIntRange(tc.min, tc.max); err != nil || got != tc.want {
				t.Fatalf("range [%d,%d]: got=%d want=%d error=%v", tc.min, tc.max, got, tc.want, err)
			}
		})
	}
}
