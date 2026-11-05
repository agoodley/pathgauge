// Package logging builds the structured logger shared by every role.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options selects the log level and output format.
type Options struct {
	// Level is debug, info, warn or error. Empty means info.
	Level string
	// Format is auto, text or json. Empty means auto: text, with timestamps
	// omitted when running under journald (which adds its own).
	Format string
}

// New returns a logger writing to w.
func New(w io.Writer, opts Options) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	hopts := &slog.HandlerOptions{Level: level}

	switch strings.ToLower(opts.Format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, hopts)), nil
	case "", "auto":
		if underJournald() {
			hopts.ReplaceAttr = dropTime
		}
		return slog.New(slog.NewTextHandler(w, hopts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, hopts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (want auto, text or json)", opts.Format)
	}
}

// ParseLevel parses a level name; empty means info.
func ParseLevel(s string) (slog.Level, error) {
	if s == "" {
		return slog.LevelInfo, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", s)
	}
	return level, nil
}

func underJournald() bool {
	return os.Getenv("JOURNAL_STREAM") != ""
}

func dropTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
