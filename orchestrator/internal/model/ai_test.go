package model_test

import (
	"math"
	"testing"

	"flexie.io/sag/internal/model"
)

// CallCost derives a model call's dollar cost from its token counts and the
// model's per-million pricing, and is zero when a model has no pricing.
func TestAIModelCallCost(t *testing.T) {
	m := &model.AIModel{InputPricePer1M: 3, OutputPricePer1M: 15}
	// 1,000,000 input at $3/M ($3.00) plus 100,000 output at $15/M ($1.50).
	if got, want := m.CallCost(1_000_000, 100_000), 4.5; math.Abs(got-want) > 1e-9 {
		t.Fatalf("CallCost = %v, want %v", got, want)
	}

	// Input and output are priced independently.
	if got, want := m.CallCost(0, 200_000), 3.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("output-only CallCost = %v, want %v", got, want)
	}

	// A model with no pricing costs nothing rather than guessing.
	if got := (&model.AIModel{}).CallCost(5000, 5000); got != 0 {
		t.Fatalf("CallCost with no pricing = %v, want 0", got)
	}
}
