// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package websearch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubBackend struct{ url string }

func (stubBackend) Kind() Kind                                      { return KindAgentCore }
func (stubBackend) Check(context.Context) error                     { return nil }
func (stubBackend) Search(context.Context, Query) ([]Result, error) { return nil, nil }

func TestTheBackendIsRebuiltOnlyWhenTheGatewayChanges(t *testing.T) {
	cfg := Config{Kind: KindAgentCore, Enabled: true, GatewayURL: "https://a/mcp"}
	builds := 0
	var buildErr error
	svc := NewService(
		func(context.Context) (Config, bool, error) { return cfg, true, nil },
		func(_ context.Context, c Config) (Backend, error) {
			builds++
			if buildErr != nil {
				return nil, buildErr
			}
			return stubBackend{url: c.GatewayURL}, nil
		}, 0, nil)

	if svc.Current().Serving() {
		t.Fatal("serving before the first reload")
	}
	for range 3 {
		if err := svc.Reload(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 || !svc.Current().Serving() {
		t.Fatalf("builds=%d serving=%v", builds, svc.Current().Serving())
	}

	// Switching it off keeps the backend and stops serving.
	cfg.Enabled = false
	_ = svc.Reload(t.Context())
	if svc.Current().Serving() || builds != 1 {
		t.Fatalf("off: builds=%d serving=%v", builds, svc.Current().Serving())
	}

	// A new gateway that fails to build is published as a failure, not
	// papered over with the old one.
	cfg.Enabled, cfg.GatewayURL, buildErr = true, "https://b/mcp", errors.New("bad gateway")
	if err := svc.Reload(t.Context()); err == nil {
		t.Fatal("a failed build was not reported")
	}
	st := svc.Current()
	if st.Serving() || st.BuildErr == nil || !strings.Contains(st.BuildErr.Error(), "bad gateway") {
		t.Fatalf("after a failed build: %+v", st)
	}

	buildErr = nil
	_ = svc.Reload(t.Context())
	if b, ok := svc.Current().Backend.(stubBackend); !ok || b.url != "https://b/mcp" {
		t.Fatalf("not rebuilt for the new gateway: %+v", svc.Current())
	}
}

func TestANilServiceServesNothing(t *testing.T) {
	var svc *Service
	if svc.Current().Serving() || svc.Reload(t.Context()) != nil {
		t.Fatal("a nil service should hold nothing and reload nothing")
	}
}

func TestAQueryIsCheckedBeforeItIsSent(t *testing.T) {
	if (Query{Text: "  "}).Validate() == nil {
		t.Error("an empty query was accepted")
	}
	if (Query{Text: strings.Repeat("é", MaxQueryChars)}).Validate() != nil {
		t.Error("the limit is in characters, not bytes")
	}
	if (Query{Text: strings.Repeat("x", MaxQueryChars+1)}).Validate() == nil {
		t.Error("an over-long query was accepted")
	}
}
