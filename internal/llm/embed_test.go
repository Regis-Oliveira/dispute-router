package llm

import (
	"math"
	"testing"
)

func TestNormaliseMakesUnitVectors(t *testing.T) {
	vec := normalise([]float32{3, 4, 0})
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if math.Abs(sum-1) > 1e-6 {
		t.Errorf("length squared = %v, want 1", sum)
	}

	// A zero vector has no direction to preserve, and dividing by its length
	// would produce NaNs that pgvector accepts and the index then sorts
	// nonsensically.
	if got := normalise([]float32{0, 0, 0}); got[0] != 0 {
		t.Errorf("a zero vector came back as %v", got)
	}
}
