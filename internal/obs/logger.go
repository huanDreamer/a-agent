// Package obs wires observability primitives: structured logging.
package obs

import (
	"fmt"
	"io"
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
	return NewLoggerTo(os.Stdout, level, format)
}

// NewLoggerTo is NewLogger with an explicit sink.
//
// It exists for the commands whose standard output is a *product* rather than a
// log: `huan-agent run` writes the model's answer to stdout so it can be piped,
// and a log line landing in the middle of that answer would corrupt it. Those
// commands point the logger at stderr instead, which is where everything that is
// not the answer belongs.
//
// The global logger is still replaced, so packages that log through zap.L() write
// to the same sink as the caller's own logger.
func NewLoggerTo(w io.Writer, level, format string) (*zap.Logger, error) {
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

	if w == nil {
		w = io.Discard
	}
	core := zapcore.NewCore(enc, zapcore.Lock(zapcore.AddSync(w)), lvl)
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
