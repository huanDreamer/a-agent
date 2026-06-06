// Package obs wires observability primitives: structured logging.
package obs

import (
	"fmt"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// NewLogger constructs a zap.Logger from a level and format string.
//
//   - level:  "debug" | "info" | "warn" | "error" (default "info")
//   - format: "json" | "console" (default "console")
//
// The returned logger is also registered as the global logger
// (zap.L() and zap.S() will use it). The caller is responsible for
// calling Sync() at shutdown.
func NewLogger(level, format string) (*zap.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	var enc zapcore.Encoder
	switch format {
	case "json", "":
		enc = zapcore.NewJSONEncoder(encCfg)
	case "console":
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		enc = zapcore.NewConsoleEncoder(encCfg)
	default:
		return nil, fmt.Errorf("invalid log format %q (want json|console)", format)
	}

	core := zapcore.NewCore(enc, zapcore.Lock(os.Stdout), lvl)
	logger := zap.New(core, zap.AddCaller())
	zap.ReplaceGlobals(logger)
	return logger, nil
}

func parseLevel(s string) (zapcore.Level, error) {
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(s)); err != nil {
		return lvl, fmt.Errorf("invalid log level %q (want debug|info|warn|error)", s)
	}
	return lvl, nil
}
