// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e && live

// The checks that need real AWS credentials.
//
// They are separated by a build tag rather than by a skip because they cost
// money, and a suite that spends by default is one somebody runs by accident.
// Everything a fake can prove is proved without this tag; what is here is what
// is true of Amazon rather than of this code.
//
// The whole file asks for one output token per request.
package e2e

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/providers/bedrock"
)

// liveRegion is the region under test, and its absence is what gates the file.
func liveRegion(t *testing.T) string {
	t.Helper()

	region := strings.TrimSpace(os.Getenv("PANORAMA_TEST_AWS_REGION"))
	if region == "" {
		t.Skip("set PANORAMA_TEST_AWS_REGION to run the live checks")
	}
	return region
}

// oneToken is the smallest useful request: a greeting, and a cap of one output
// token. Everything in this file sends it.
var oneToken = []byte(`{"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)

func liveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestLiveCatalogue reads what Bedrock publishes, and checks the three things
// this product derives from it.
func TestLiveCatalogue(t *testing.T) {
	region := liveRegion(t)
	ctx := liveContext(t)

	cat, err := bedrock.NewCatalogue(ctx, bedrock.Config{Region: region})
	if err != nil {
		t.Fatalf("building the catalogue source: %v", err)
	}

	entries, err := cat.Catalogue(ctx)
	if err != nil {
		t.Fatalf("discovery failed against %s: %v", region, err)
	}
	if len(entries) == 0 {
		t.Fatalf("no models discovered in %s", region)
	}

	var (
		anthropic  int
		streaming  int
		profiles   int
		bareIDs    int
		legacyBare int
		withRegion int
	)
	for _, e := range entries {
		if !strings.EqualFold(e.Vendor, "anthropic") {
			continue
		}
		anthropic++
		if e.Streaming {
			streaming++
		}
		if e.Geography != "" {
			profiles++
			if len(e.Regions) > 0 {
				withRegion++
			}
		} else {
			bareIDs++
			if strings.EqualFold(e.Lifecycle, "LEGACY") {
				legacyBare++
			}
		}
	}

	// Named rather than positional. Every one of these is a %d, so a misordered
	// argument list still compiles, still passes vet, and reports a catalogue
	// that does not exist - which is worse than reporting nothing, because the
	// assertions below are right and only the explanation is wrong.
	t.Logf("%s: %d Anthropic entries - streaming=%d profiles=%d profiles_with_regions=%d bare_ids=%d bare_legacy=%d",
		region, anthropic, streaming, profiles, withRegion, bareIDs, legacyBare)

	if streaming == 0 {
		t.Error("no Anthropic model in this region will stream; nothing here can serve a coding agent")
	}
	// A profile's model ARNs enumerate the regions it may route to, and that is
	// the data-residency answer. It cannot be derived from anything else, so a
	// profile without one is a gap in what an operator can be told.
	if profiles > 0 && withRegion != profiles {
		t.Errorf("%d of %d profiles carry no regions; residency cannot be answered for them",
			profiles-withRegion, profiles)
	}
	// The rule the console's ordering is built on: bare ids are the exception,
	// not the norm. If this stops holding, leading with a profile stops being
	// the right default.
	if bareIDs >= profiles {
		t.Errorf("%d bare ids against %d profiles; the catalogue's shape has changed", bareIDs, profiles)
	}
}

// preferredFamily is the model the live checks invoke.
//
// Named rather than derived. "The cheapest current model" in ap-southeast-1 is
// apac.anthropic.claude-3-haiku-20240307-v1:0 - a 2024 model that still answers,
// is not marked LEGACY, and that no deployment routes to. A credential proved
// against something nobody serves is not proof of much, and the beta flags a
// current client sends are not the ones that model accepts.
//
// It is a family rather than an id, because a geography prefix and a revision
// date are exactly what the provider's own table strips before a lookup, so one
// constant covers apac., global. and a revision AWS makes in place.
const preferredFamily = "anthropic.claude-haiku-4-5"

// liveTarget picks the model to invoke, and refuses to improvise.
func liveTarget(ctx context.Context, t *testing.T, region string) string {
	t.Helper()

	cat, err := bedrock.NewCatalogue(ctx, bedrock.Config{Region: region})
	if err != nil {
		t.Fatalf("building the catalogue source: %v", err)
	}
	entries, err := cat.Catalogue(ctx)
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}

	// Sorted, so two tests in one run pick the same model.
	//
	// Without this the catalogue's order decides, and a run where the
	// invocation check and the beta check happened to choose differently is one
	// where the two results cannot be compared - which is exactly the state
	// somebody is in when one passes and the other reports AccessDenied.
	slices.SortFunc(entries, func(a, b providers.CatalogueEntry) int {
		return strings.Compare(a.ID, b.ID)
	})

	var offered []string
	for _, e := range entries {
		if !strings.EqualFold(e.Vendor, "anthropic") || !e.Streaming {
			continue
		}
		if strings.EqualFold(e.Lifecycle, "LEGACY") {
			continue
		}
		// It has to be reached through an inference profile, which is how a
		// route actually reaches it: the bare id is the exception, and here it
		// is the one model this test must not pick.
		if e.Geography == "" {
			continue
		}
		offered = append(offered, e.ID)
		if strings.HasPrefix(e.FoundationModelID, preferredFamily) {
			return e.ID
		}
	}

	// Refused rather than substituted. Falling back to whatever else is current
	// would put the checks back on a model nobody chose, and the whole point of
	// naming one is that a run says which model it proved.
	t.Fatalf("no %s inference profile in %s; the profiles offered are %v",
		preferredFamily, region, offered)
	return ""
}

// TestLiveInvocation is the one thing no amount of API inspection can answer.
//
// GetFoundationModelAvailability reporting NOT_AVAILABLE means "nobody has
// invoked this here yet", not "you are blocked", and an SCP denying
// bedrock:InvokeModel is invisible to it. One real request is what separates a
// working deployment from a plausible one.
func TestLiveInvocation(t *testing.T) {
	region := liveRegion(t)
	ctx := liveContext(t)

	target := liveTarget(ctx, t, region)
	t.Logf("invoking %s in %s", target, region)

	p, err := bedrock.New(ctx, bedrock.Config{Region: region})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	stream, err := p.InvokeStream(ctx, &providers.Request{Model: target, Body: oneToken})
	if err != nil {
		t.Fatalf("streaming from %s: %v", target, err)
	}
	defer func() { _ = stream.Close() }()

	events := 0
	for {
		if _, rerr := stream.Next(); errors.Is(rerr, io.EOF) {
			break
		} else if rerr != nil {
			t.Fatalf("reading the stream: %v", rerr)
		}
		events++
	}

	usage := stream.Usage()
	t.Logf("%d events, usage %+v", events, usage)

	if events == 0 {
		t.Error("the stream carried no events")
	}
	// Input is always reported. Output may legitimately be zero at max_tokens 1
	// if the first token is the stop, so it is logged rather than asserted.
	if usage.InputTokens <= 0 {
		t.Errorf("no input tokens reported; the accounting every number rests on is missing")
	}
}

// TestLiveUnsupportedBetasAreStillUnsupported checks a hardcoded list against
// the service it describes, in both directions.
//
// DefaultUnsupportedBetas is dropped from every request. If Bedrock has since
// started accepting one of these, the product is silently removing a feature
// somebody is paying for; if it has started refusing one that is not on the
// list, every request carrying it fails outright. Neither shows up anywhere
// else, and both are one request each to find out.
func TestLiveUnsupportedBetasAreStillUnsupported(t *testing.T) {
	region := liveRegion(t)
	ctx := liveContext(t)
	target := liveTarget(ctx, t, region)
	// Logged because this test and the invocation check pick their own target,
	// and a reader comparing "one worked and one did not" needs to know whether
	// they were even talking to the same model.
	t.Logf("probing beta flags against %s in %s", target, region)

	// An explicitly empty list forwards everything, which is the setting this
	// check needs: the product's own filtering would hide the answer.
	raw, err := bedrock.New(ctx, bedrock.Config{Region: region, UnsupportedBetas: []string{}})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	invoke := func(betas ...string) error {
		_, ierr := raw.Invoke(ctx, &providers.Request{Model: target, Body: oneToken, Betas: betas})
		return ierr
	}

	t.Run("each refused flag is still refused", func(t *testing.T) {
		for _, beta := range bedrock.DefaultUnsupportedBetas {
			t.Run(beta, func(t *testing.T) {
				if err := invoke(beta); err == nil {
					t.Errorf("Bedrock now accepts %q; dropping it removes a feature that works", beta)
				} else {
					t.Logf("still refused: %v", err)
				}
			})
		}
	})

	t.Run("a request carrying none of them is served", func(t *testing.T) {
		// The control, and it has already earned its place: a role without
		// aws-marketplace:Subscribe refuses every invocation with AccessDenied,
		// which reads as "the flag was refused" in each subtest above and
		// records four passes that prove nothing. Only this one notices.
		if err := invoke(); err != nil {
			t.Fatalf("a request with no beta flags failed, so the results above mean nothing: %v", err)
		}
	})
}
