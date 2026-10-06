// SPDX-License-Identifier: GPL-3.0-or-later

// Package logx configures levelled logging (built on log/slog) with an extra
// TRACE level for wire-level hex dumps.
package logx

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// LevelTrace is below Debug and enables hex dumps of every packet.
const LevelTrace = slog.Level(-8)

var level = new(slog.LevelVar)

// Setup installs a compact text handler on w as the slog default.
// verbosity: 0 = info, 1 = debug, 2+ = trace.
func Setup(w io.Writer, verbosity int) {
	switch {
	case verbosity >= 2:
		level.Set(LevelTrace)
	case verbosity == 1:
		level.Set(slog.LevelDebug)
	default:
		level.Set(slog.LevelInfo)
	}
	slog.SetDefault(slog.New(&handler{mu: &sync.Mutex{}, w: w}))
}

// Enabled reports whether messages at lvl are emitted.
func Enabled(lvl slog.Level) bool { return lvl >= level.Level() }

// TraceEnabled reports whether hex dumps should be produced.
func TraceEnabled() bool { return Enabled(LevelTrace) }

// Trace logs at trace level.
func Trace(l *slog.Logger, msg string, args ...any) {
	l.Log(context.Background(), LevelTrace, msg, args...)
}

// Dump returns an indented hex dump of b suitable for appending to a log line.
func Dump(b []byte) string {
	d := strings.TrimRight(hex.Dump(b), "\n")
	return "\n    " + strings.ReplaceAll(d, "\n", "\n    ")
}

// Component returns a logger tagged with a component name.
func Component(name string) *slog.Logger { return slog.Default().With("c", name) }

// handler renders "15:04:05.000 LEVEL [component] message key=value ...".
type handler struct {
	mu    *sync.Mutex // shared by all derived handlers
	w     io.Writer
	attrs []slog.Attr
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= level.Level() }

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	comp, stamp := "", r.Time.Format("15:04:05.000")
	var rest []slog.Attr
	collect := func(a slog.Attr) bool {
		switch {
		case a.Key == "c" && comp == "":
			comp = a.Value.String()
		case a.Key == "t": // explicit timestamp (e.g. capture-relative) replaces wall clock
			stamp = a.Value.String()
		default:
			rest = append(rest, a)
		}
		return true
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(collect)
	b.WriteString(stamp)
	b.WriteByte(' ')
	b.WriteString(levelName(r.Level))
	if comp != "" {
		fmt.Fprintf(&b, " [%s]", comp)
	}
	b.WriteByte(' ')
	b.WriteString(r.Message)
	var dumps []string
	for _, a := range rest {
		v := a.Value.Resolve()
		if a.Key == "hex" {
			dumps = append(dumps, v.String())
			continue
		}
		s := v.String()
		if v.Kind() == slog.KindDuration {
			s = v.Duration().Round(time.Millisecond).String()
		}
		if s == "" || strings.ContainsAny(s, " \t\"=") {
			s = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(&b, " %s=%s", a.Key, s)
	}
	for _, d := range dumps {
		b.WriteString(d)
	}
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &handler{mu: h.mu, w: h.w, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *handler) WithGroup(string) slog.Handler { return h }

func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "TRACE"
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO "
	case l < slog.LevelError:
		return "WARN "
	default:
		return "ERROR"
	}
}

// Stderr is a convenience for Setup(os.Stderr, v).
func Stderr(verbosity int) { Setup(os.Stderr, verbosity) }
