package chat

import "testing"

// TestCalibrationLearnsTheRealRatio is the property that makes a large window
// safe: the estimator is only trusted until the provider says what it actually
// counted, and then the difference is carried into the next step's budget.
func TestCalibrationLearnsTheRealRatio(t *testing.T) {
	var c windowCalibration
	// Nothing observed yet: the overhead is just the schemas.
	if got := c.overhead(100_000, 4_000); got != 4_000 {
		t.Fatalf("before any observation overhead = %d, want the schema cost alone", got)
	}

	// The provider counted 1.7× what the turn predicted.
	c.observe(170_000, 100_000)
	got := c.overhead(100_000, 4_000)
	if got <= 4_000 {
		t.Fatalf("overhead = %d, want the under-count added", got)
	}
	if got < 70_000 || got > 75_000 {
		t.Errorf("overhead = %d, want about 70000 + 4000", got)
	}

	// A provider counting *less* than predicted never lowers the budget: firing
	// early costs a summary call, firing late costs the turn.
	c2 := windowCalibration{}
	c2.observe(50_000, 100_000)
	if got := c2.overhead(100_000, 0); got != 0 {
		t.Errorf("overhead = %d, want 0: an under-count must not buy more window", got)
	}

	// A nonsense ratio is bounded, not followed: a provider reporting something
	// unrelated must not compress the window to nothing.
	c3 := windowCalibration{}
	c3.observe(10_000_000, 1_000)
	if c3.ratio > maxCalibrationRatio {
		t.Errorf("ratio = %v, want it bounded at %v", c3.ratio, maxCalibrationRatio)
	}
}

// TestCalibrationIgnoresMissingUsage: a provider that reports no token counts
// (some do, for a stream) leaves the budget on the estimate rather than on a
// division by zero.
func TestCalibrationIgnoresMissingUsage(t *testing.T) {
	var c windowCalibration
	c.observe(0, 1_000)
	c.observe(1_000, 0)
	if c.observed {
		t.Fatal("an unusable observation was recorded")
	}
	if got := c.overhead(50_000, 100); got != 100 {
		t.Errorf("overhead = %d, want the schemas alone", got)
	}
}
