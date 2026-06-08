package builtin

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/Knetic/govaluate"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// CalcInput is the parameter schema for the "calc" tool.
type CalcInput struct {
	Expression string `json:"expression" jsonschema:"description=Math expression to evaluate, e.g. (1+2)*3, required"`
}

// CalcOutput is what the calc tool returns.
type CalcOutput struct {
	Result float64 `json:"result"`
}

// calcFunctions registers a small set of math helpers with govaluate.
// Note: govaluate uses ^ for bitwise XOR, not exponentiation. Callers
// that need x^y should use pow(x, y).
var calcFunctions = map[string]govaluate.ExpressionFunction{
	"abs":   func(args ...any) (any, error) { return unaryFloat(args, math.Abs) },
	"ceil":  func(args ...any) (any, error) { return unaryFloat(args, math.Ceil) },
	"floor": func(args ...any) (any, error) { return unaryFloat(args, math.Floor)},
	"round": func(args ...any) (any, error) { return unaryFloat(args, math.Round) },
	"sqrt":  func(args ...any) (any, error) { return unaryFloat(args, math.Sqrt) },
	"log":   func(args ...any) (any, error) { return unaryFloat(args, math.Log) },
	"log10": func(args ...any) (any, error) { return unaryFloat(args, math.Log10) },
	"exp":   func(args ...any) (any, error) { return unaryFloat(args, math.Exp) },
	"pow":   binaryFloatPow,
	"max":   binaryFloat(math.Max),
	"min":   binaryFloat(math.Min),
}

func unaryFloat(args []any, fn func(float64) float64) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expected 1 argument, got %d", len(args))
	}
	x, err := toFloat64(args[0])
	if err != nil {
		return nil, err
	}
	return fn(x), nil
}

func binaryFloat(fn func(a, b float64) float64) govaluate.ExpressionFunction {
	return func(args ...any) (any, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("expected 2 arguments, got %d", len(args))
		}
		a, err := toFloat64(args[0])
		if err != nil {
			return nil, err
		}
		b, err := toFloat64(args[1])
		if err != nil {
			return nil, err
		}
		return fn(a, b), nil
	}
}

func binaryFloatPow(args ...any) (any, error) {
	return binaryFloat(math.Pow)(args...)
}

// NewCalcTool returns a Tool that evaluates a simple arithmetic
// expression and returns the numeric result. Supported: + - * /,
// parentheses, numeric literals, and helper functions (abs, ceil,
// floor, round, sqrt, log, log10, exp, pow, max, min). Variables are
// not allowed — the expression must be self-contained.
func NewCalcTool() (tool.InvokableTool, error) {
	return utils.InferTool("calc",
		"Evaluate a math expression. Supports + - * /, parentheses, and functions: abs ceil floor round sqrt log log10 exp pow max min. Example: (1+2)*3 + sqrt(16)",
		func(_ context.Context, in CalcInput) (CalcOutput, error) {
			expr := strings.TrimSpace(in.Expression)
			if expr == "" {
				return CalcOutput{}, fmt.Errorf("expression is required")
			}
			g, err := govaluate.NewEvaluableExpressionWithFunctions(expr, calcFunctions)
			if err != nil {
				return CalcOutput{}, fmt.Errorf("parse expression: %w", err)
			}
			v, err := g.Evaluate(nil)
			if err != nil {
				return CalcOutput{}, fmt.Errorf("evaluate expression: %w", err)
			}
			f, err := toFloat64(v)
			if err != nil {
				return CalcOutput{}, fmt.Errorf("expression returned non-numeric value: %w", err)
			}
			return CalcOutput{Result: f}, nil
		})
}

func toFloat64(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		var f float64
		if _, err := fmt.Sscan(x, &f); err != nil {
			return 0, fmt.Errorf("cannot convert %q to number", x)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}
