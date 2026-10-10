// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Command fakebedrock serves Amazon Bedrock's wire protocol, for development.
//
// The end-to-end harness embeds internal/fakebedrock directly; this exists for
// the other case, which is looking at the console and driving a coding tool on a
// machine with no AWS credentials at all:
//
//	go run ./cmd/fakebedrock -addr :9099
//	AWS_ENDPOINT_URL_BEDROCK_RUNTIME=http://127.0.0.1:9099 \
//	AWS_ENDPOINT_URL_BEDROCK=http://127.0.0.1:9099 \
//	AWS_ACCESS_KEY_ID=fake AWS_SECRET_ACCESS_KEY=fake \
//	PANORAMA_BEDROCK_REGION=us-east-1 make dev
//
// It answers every model with the same short completion and the same fixed
// accounting, so spend accumulates predictably and `panorama models discover`
// returns a catalogue a route can point at.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/dnxlabs/panorama/internal/fakebedrock"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fakebedrock: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:9099", "address to listen on")
	text := flag.String("text", "OK", "the assistant text every completion returns")
	delay := flag.Duration("frame-delay", 0, "pause between streamed events, to watch a stream arrive")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	srv := &http.Server{
		Addr: *addr,
		Handler: fakebedrock.New(
			fakebedrock.WithText(*text),
			fakebedrock.WithFrameDelay(*delay),
		),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("fake bedrock listening - this signs nothing and verifies nothing",
		slog.String("addr", *addr),
		slog.String("runtime_endpoint", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME=http://"+*addr),
		slog.String("control_endpoint", "AWS_ENDPOINT_URL_BEDROCK=http://"+*addr),
	)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}
	return nil
}
