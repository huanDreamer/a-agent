package builtin

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// TimeInput is the parameter schema for the "time" tool. It is
// exported so the generated JSON schema documents the optional
// timezone field.
type TimeInput struct {
	Timezone string `json:"timezone" jsonschema:"description=IANA timezone name such as Asia/Shanghai. Empty returns the server local zone"`
}

// TimeOutput is what the time tool returns.
type TimeOutput struct {
	Timezone string `json:"timezone"`
	Now      string `json:"now"`
	Unix     int64  `json:"unix"`
}

// NewTimeTool returns a Tool that reports the current time. When
// TimeInput.Timezone is non-empty it must be a valid IANA name; an
// empty string falls back to the server's local zone.
func NewTimeTool() (tool.InvokableTool, error) {
	return utils.InferTool("time",
		"Return the current time. Optionally accepts an IANA timezone (e.g. Asia/Shanghai); empty string = server local zone.",
		func(_ context.Context, in TimeInput) (TimeOutput, error) {
			loc := time.Local
			if in.Timezone != "" {
				l, err := time.LoadLocation(in.Timezone)
				if err != nil {
					return TimeOutput{}, fmt.Errorf("invalid timezone %q: %w", in.Timezone, err)
				}
				loc = l
			}
			now := time.Now().In(loc)
			return TimeOutput{
				Timezone: loc.String(),
				Now:      now.Format(time.RFC3339),
				Unix:     now.Unix(),
			}, nil
		})
}
