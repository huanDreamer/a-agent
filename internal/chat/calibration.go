package chat

import (
	"math"

	"github.com/cloudwego/eino/schema"

	gctx "github.com/huan/huan-agent/internal/context"
)

// estimateMessages is the message half of the estimate, so the observation and
// the correction are computed from one implementation.
func estimateMessages(msgs []*schema.Message) int { return gctx.DefaultEstimator(msgs) }

// Calibrating the window estimate against what the provider says it counted.
//
// The estimator in internal/context approximates tokens from characters, and no
// approximation is exact: measured against a real turn, the provider's own count
// was 1.7× the estimate even after the rune-aware rewrite. On a 128k model that
// only means the window fills later than intended; on the 1M-token window a
// gateway now reports for a frontier model, 1.7 × 0.7 is 1.2 — the history would
// pass the model's real limit and the turn would end in a context-limit error.
//
// The fix is to stop guessing about the ratio: every model call reports the
// prompt tokens it actually counted, so the ratio between that and what this
// turn predicted is measured, and the next step's budget is adjusted by it. The
// window then fills as tightly as the real numbers allow, which is the point of
// sizing it from the model's window in the first place.
//
// It is deliberately one-sided: the ratio is never taken below 1, because
// under-counting is the failure that breaks a turn while over-counting merely
// compresses a little earlier than necessary.

// maxCalibrationRatio bounds the correction. A ratio above this is not an
// estimate error but a bug (a provider counting something else entirely), and
// letting it through would compress the window down to nothing.
const maxCalibrationRatio = 3.0

// calibrationRatioWeight is how much of a new observation is believed. Half:
// the ratio moves with the content (a window full of Chinese prose and one full
// of Go source differ), so it should follow, but one odd reading should not
// swing the budget.
const calibrationRatioWeight = 0.5

// windowCalibration tracks the provider's token count against this package's
// estimate for the same window.
type windowCalibration struct {
	// ratio is real/estimated for the last step, smoothed. 1 means the estimate
	// is believed as-is.
	ratio float64
	// observed is whether any model call has reported usage. Without one the
	// ratio stays 1 and the budget is the estimate's own.
	observed bool
}

// observe folds one model call's reported prompt tokens into the ratio.
//
// estimated is what this turn predicted for the same window: the message
// estimate plus the tool schemas.
func (c *windowCalibration) observe(reportedTokens, estimatedTokens int) {
	if reportedTokens <= 0 || estimatedTokens <= 0 {
		return
	}
	ratio := float64(reportedTokens) / float64(estimatedTokens)
	if ratio < 1 {
		// The provider counted fewer tokens than predicted. Recording that would
		// let the window grow past what the estimate allowed, for no benefit: a
		// turn that compresses slightly early is a turn that works.
		ratio = 1
	}
	if ratio > maxCalibrationRatio {
		ratio = maxCalibrationRatio
	}
	if !c.observed {
		c.ratio = ratio
		c.observed = true
		return
	}
	c.ratio = c.ratio*(1-calibrationRatioWeight) + ratio*calibrationRatioWeight
}

// overhead is the extra tokens the condenser should be told about: the part of
// the window the estimator under-counts, plus the schemas it cannot see at all.
//
// It is 0 when nothing has been observed yet (the first step of a turn), which
// is the pre-calibration behaviour.
func (c *windowCalibration) overhead(estimatedMessages, schemaTokens int) int {
	extra := 0
	if c.observed && c.ratio > 1 && estimatedMessages > 0 {
		extra = int(math.Round(float64(estimatedMessages) * (c.ratio - 1)))
	}
	return schemaTokens + extra
}
