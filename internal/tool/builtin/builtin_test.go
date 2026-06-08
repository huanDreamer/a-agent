package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func invoke(t *testing.T, it tool.InvokableTool, args string) string {
	t.Helper()
	out, err := it.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out
}

func TestTimeTool_Default(t *testing.T) {
	it, err := NewTimeTool()
	if err != nil {
		t.Fatal(err)
	}
	out := invoke(t, it, `{}`)
	if !strings.Contains(out, `"now"`) || !strings.Contains(out, `"unix"`) {
		t.Errorf("output missing fields: %s", out)
	}
}

func TestTimeTool_WithTimezone(t *testing.T) {
	it, _ := NewTimeTool()
	out := invoke(t, it, `{"timezone":"Asia/Shanghai"}`)
	if !strings.Contains(out, "Asia/Shanghai") {
		t.Errorf("missing timezone in output: %s", out)
	}
}

func TestTimeTool_BadTimezone(t *testing.T) {
	it, _ := NewTimeTool()
	_, err := it.InvokableRun(context.Background(), `{"timezone":"Not/A/Zone"}`)
	if err == nil {
		t.Error("expected error for invalid timezone")
	}
}

func TestCalcTool_Simple(t *testing.T) {
	it, _ := NewCalcTool()
	out := invoke(t, it, `{"expression":"(1+2)*3"}`)
	if !strings.Contains(out, `"result":9`) {
		t.Errorf("output = %s, want result 9", out)
	}
}

func TestCalcTool_Pow(t *testing.T) {
	it, _ := NewCalcTool()
	out := invoke(t, it, `{"expression":"pow(2, 10)"}`)
	if !strings.Contains(out, `"result":1024`) {
		t.Errorf("output = %s, want result 1024", out)
	}
}

func TestCalcTool_Sqrt(t *testing.T) {
	it, _ := NewCalcTool()
	out := invoke(t, it, `{"expression":"sqrt(16)"}`)
	if !strings.Contains(out, `"result":4`) {
		t.Errorf("output = %s, want result 4", out)
	}
}

func TestCalcTool_Empty(t *testing.T) {
	it, _ := NewCalcTool()
	if _, err := it.InvokableRun(context.Background(), `{"expression":""}`); err == nil {
		t.Error("expected error for empty expression")
	}
}

func TestCalcTool_Invalid(t *testing.T) {
	it, _ := NewCalcTool()
	if _, err := it.InvokableRun(context.Background(), `{"expression":"1++"}`); err == nil {
		t.Error("expected error for invalid expression")
	}
}

func TestEchoTool(t *testing.T) {
	it, _ := NewEchoTool()
	out := invoke(t, it, `{"text":"hello"}`)
	if !strings.Contains(out, `"text":"hello"`) {
		t.Errorf("output = %s", out)
	}
}

func TestEchoTool_Empty(t *testing.T) {
	it, _ := NewEchoTool()
	if _, err := it.InvokableRun(context.Background(), `{"text":""}`); err == nil {
		t.Error("expected error for empty text")
	}
}

func TestCalcTool_MaxMinAbs(t *testing.T) {
	it, _ := NewCalcTool()
	cases := []struct {
		expr string
		want string
	}{
		{"max(3, 7)", `"result":7`},
		{"min(3, 7)", `"result":3`},
		{"abs(-2.5)", `"result":2.5`},
		{"ceil(1.1)", `"result":2`},
		{"floor(1.9)", `"result":1`},
		{"round(1.5)", `"result":2`},
		{"log10(1000)", `"result":3`},
		{"exp(0)", `"result":1`},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			out := invoke(t, it, `{"expression":"`+tc.expr+`"}`)
			if !strings.Contains(out, tc.want) {
				t.Errorf("expr %s = %s, want to contain %s", tc.expr, out, tc.want)
			}
		})
	}
}

func TestCalcTool_BadArity(t *testing.T) {
	it, _ := NewCalcTool()
	if _, err := it.InvokableRun(context.Background(), `{"expression":"max(1)"}`); err == nil {
		t.Error("expected error for max with 1 arg")
	}
	if _, err := it.InvokableRun(context.Background(), `{"expression":"abs()"}`); err == nil {
		t.Error("expected error for abs with 0 args")
	}
}
