// SPDX-License-Identifier: GPL-3.0-or-later

package logx

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatAndLevels(t *testing.T) {
	var buf bytes.Buffer
	Setup(&buf, 0)
	l := Component("test")
	l.Info("hello world", "k", "v w", "n", 3)
	l.Debug("hidden")
	Trace(l, "hidden too")
	line := buf.String()
	if !strings.Contains(line, "INFO  [test] hello world k=\"v w\" n=3") {
		t.Fatalf("unexpected format: %q", line)
	}
	if strings.Contains(line, "hidden") {
		t.Fatal("debug/trace emitted at info level")
	}

	buf.Reset()
	Setup(&buf, 2)
	l = Component("x")
	if !TraceEnabled() {
		t.Fatal("trace not enabled at -vv")
	}
	Trace(l, "dump", "t", "  1.500", "hex", Dump([]byte{0xde, 0xad}))
	out := buf.String()
	if !strings.HasPrefix(out, "  1.500 TRACE [x] dump") || !strings.Contains(out, "de ad") {
		t.Fatalf("trace output: %q", out)
	}
}
