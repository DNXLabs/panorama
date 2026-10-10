// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package logging builds the process logger from configuration.
//
// Every log line Panorama emits is structured. Once identity and routing
// context exist, each line carries userId, teamId, sessionId, model, provider,
// and tool wherever they are known - attached as slog attributes rather than
// interpolated into messages, so they stay queryable.
package logging

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/dnxlabs/panorama/internal/config"
)

// New builds a logger for the given configuration, writing to w.
func New(cfg config.Log, w io.Writer) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch cfg.Format {
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	case "text":
		handler = slog.NewTextHandler(w, opts)
	default:
		// Validation should have caught this, so reaching here means a caller
		// built a Log value by hand. Fail rather than pick one.
		return nil, fmt.Errorf("logging: unsupported format %q (want json or text)", cfg.Format)
	}

	return slog.New(handler), nil
}

func parseLevel(name string) (slog.Level, error) {
	switch name {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unsupported level %q (want debug, info, warn or error)", name)
	}
}
