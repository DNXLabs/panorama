// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsbedrock "github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"

	"github.com/dnxlabs/panorama/internal/providers"
)

// Catalogue reports what this account and region can actually invoke.
//
// It answers from two calls that have to be joined, because neither is
// sufficient alone. ListFoundationModels knows which models exist, what they
// are called, whether they stream, whether they are being retired, and - the
// part operators most often get wrong - whether the bare model id is refused in
// favour of an inference profile. ListInferenceProfiles knows which profiles
// exist and, in its Models list, which regions each one may route to.
//
// The regions are what make this worth doing. "Where may this request be
// processed" is a compliance question with a per-model answer that changes when
// AWS adds a region, and nothing but the provider knows it.
type Catalogue struct {
	models  *awsbedrock.Client
	region  string
	windows map[string]int
	// apis is the one derivation of what a model speaks here, with the
	// provider's overrides applied. The instance hands the same function to
	// the policy engine, so a row and a route cannot disagree.
	apis func(string) []providers.API
	log  *slog.Logger
}

// NewCatalogue builds a catalogue source for a region.
func NewCatalogue(ctx context.Context, conf Config) (*Catalogue, error) {
	if err := validRegion(conf.Region); err != nil {
		return nil, err
	}
	if conf.Logger == nil {
		conf.Logger = slog.New(slog.DiscardHandler)
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(conf.Region))
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config for %s: %w", conf.Region, err)
	}

	return &Catalogue{
		models:  awsbedrock.NewFromConfig(cfg),
		region:  conf.Region,
		windows: conf.ContextWindows,
		apis:    runtimeAPIs(conf.APIs),
		log:     conf.Logger,
	}, nil
}

// The catalogue's client is the one that can answer for a model's access.
var _ providers.AccessReporter = (*Catalogue)(nil)

// Region reports which region was discovered.
func (c *Catalogue) Region() string { return c.region }

// Catalogue lists every target this deployment could invoke.
//
// Both calls must succeed. A partial catalogue is worse than none: an
// administrator reading a list with arbitrary models missing has no way to tell
// it from an account that genuinely lacks them, and the obvious next action -
// enabling what is visible - quietly bakes the gap into the deployment.
func (c *Catalogue) Catalogue(ctx context.Context) ([]providers.CatalogueEntry, error) {
	foundation, err := c.foundationModels(ctx)
	if err != nil {
		return nil, err
	}
	profiles, err := c.inferenceProfiles(ctx)
	if err != nil {
		return nil, err
	}

	var (
		out    []providers.CatalogueEntry
		seen   = map[string]bool{}
		joined = map[string]bool{}
	)

	// Profiles first: for current models this is the only way in, and the entry
	// carries the region list the bare model id has no equivalent of.
	for _, p := range profiles {
		fm, ok := foundation[p.foundationModelID]
		if !ok {
			// A profile for a model this region does not list. Skipped rather
			// than invented: without the foundation model there is no display
			// name, no streaming flag and no lifecycle, and a half-populated row
			// would read as a model with no capabilities rather than as one this
			// build could not describe.
			c.log.DebugContext(ctx, "inference profile names a model not in this region",
				"profile", p.id, "model", p.foundationModelID, "region", c.region)
			continue
		}
		joined[p.foundationModelID] = true

		if seen[p.id] {
			continue
		}
		seen[p.id] = true
		profileLimits := c.limitsFor(p.foundationModelID)
		out = append(out, providers.CatalogueEntry{
			ID:                p.id,
			FoundationModelID: p.foundationModelID,
			Provider:          providers.KindBedrock,
			Region:            c.region,
			Vendor:            fm.provider,
			DisplayName:       fm.name,
			APIs:              c.apis(p.id),
			Geography:         p.geography,
			Regions:           p.regions,
			Streaming:         fm.streaming,
			Lifecycle:         fm.lifecycle,
			RequiresProfile:   fm.requiresProfile,
			ContextWindow:     profileLimits.Context,
			MaxOutputTokens:   profileLimits.Output,
		})
	}

	// Then the models that can still be invoked by their bare id. Older models
	// support this and current ones do not, so leaving them out would hide every
	// model a long-lived deployment might already be using.
	for id, fm := range foundation {
		if !fm.onDemand || seen[id] {
			continue
		}
		seen[id] = true
		bareLimits := c.limitsFor(id)
		out = append(out, providers.CatalogueEntry{
			ID:                id,
			FoundationModelID: id,
			Provider:          providers.KindBedrock,
			Region:            c.region,
			Vendor:            fm.provider,
			DisplayName:       fm.name,
			APIs:              c.apis(id),
			// No geography and no region list: a bare id is invoked in the
			// region the call is made to, which the deployment already knows.
			Streaming:       fm.streaming,
			Lifecycle:       fm.lifecycle,
			RequiresProfile: false,
			ContextWindow:   bareLimits.Context,
			MaxOutputTokens: bareLimits.Output,
		})
	}

	// A model that requires a profile and has none reachable here cannot be
	// invoked at all. Counted and logged rather than silently absent, because
	// "why is Opus not in the list" is otherwise unanswerable from the console.
	var unreachable []string
	for id, fm := range foundation {
		if fm.requiresProfile && !fm.onDemand && !joined[id] {
			unreachable = append(unreachable, id)
		}
	}
	if len(unreachable) > 0 {
		slices.Sort(unreachable)
		c.log.InfoContext(ctx, "models require an inference profile that this region does not offer",
			"region", c.region, "count", len(unreachable),
			"models", strings.Join(unreachable, ", "))
	}

	// Models this build has no context window for. Reported once per pass
	// rather than per model, and at info rather than warn: an unknown window
	// is the expected state for anything outside the Anthropic catalogue, and
	// the line exists so that a *newly* unknown Anthropic model - the case
	// that means the table has gone stale - is visible in the log the day it
	// appears rather than the day a route refusal reads wrong.
	var windowless []string
	for _, e := range out {
		if e.ContextWindow == 0 {
			windowless = append(windowless, e.ID)
		}
	}
	if len(windowless) > 0 {
		slices.Sort(windowless)
		c.log.InfoContext(ctx, "no context window recorded for these models",
			"region", c.region, "count", len(windowless),
			"models", strings.Join(windowless, ", "),
			"remedy", "set a context window for it in Settings, Model providers, if a route needs one")
	}

	slices.SortFunc(out, func(a, b providers.CatalogueEntry) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// limitsFor reports what a foundation model accepts and produces, or zeroes
// when this build has no entry for it. See windows.go for why the table exists
// at all: the provider publishes neither number.
func (c *Catalogue) limitsFor(foundationModelID string) modelLimits {
	limits, ok := limitsFor(c.windows, foundationModelID)
	if !ok {
		return modelLimits{}
	}
	return limits
}

// foundationModel is what ListFoundationModels knows about one model.
type foundationModel struct {
	name     string
	provider string
	// streaming reports whether a response can be streamed. Nothing on the hot
	// path may buffer, so this decides eligibility rather than describing it.
	streaming bool
	lifecycle string
	// onDemand reports that the bare model id is invocable directly.
	onDemand bool
	// requiresProfile reports that it is not, and a profile must be used.
	//
	// Read rather than assumed. Current Anthropic models return
	// INFERENCE_PROFILE and nothing else, which is the machine-readable form of
	// the rule that otherwise has to be learned by a request failing.
	requiresProfile bool
}

func (c *Catalogue) foundationModels(ctx context.Context) (map[string]foundationModel, error) {
	out, err := c.models.ListFoundationModels(ctx, &awsbedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, fmt.Errorf("bedrock: list foundation models in %s: %w", c.region, err)
	}

	models := make(map[string]foundationModel, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		if m.ModelId == nil {
			continue
		}
		fm := foundationModel{
			name:      derefString(m.ModelName),
			provider:  derefString(m.ProviderName),
			streaming: m.ResponseStreamingSupported != nil && *m.ResponseStreamingSupported,
		}
		if m.ModelLifecycle != nil {
			fm.lifecycle = string(m.ModelLifecycle.Status)
		}
		for _, t := range m.InferenceTypesSupported {
			switch t {
			case bedrocktypes.InferenceTypeOnDemand:
				fm.onDemand = true
			case bedrocktypes.InferenceTypeProvisioned:
				// Provisioned throughput is a purchased capacity model, not
				// something a gateway can invoke by id.
			default:
				// INFERENCE_PROFILE, and anything AWS adds later that is
				// neither of the two above. Treated as "needs a profile"
				// because that is the safe reading: the worst case is a model
				// offered only through a profile that does work.
				fm.requiresProfile = true
			}
		}
		models[*m.ModelId] = fm
	}
	return models, nil
}

// inferenceProfile is one routing target, with where it may route.
type inferenceProfile struct {
	id                string
	foundationModelID string
	geography         string
	regions           []string
}

func (c *Catalogue) inferenceProfiles(ctx context.Context) ([]inferenceProfile, error) {
	var (
		profiles []inferenceProfile
		next     *string
	)
	for {
		out, err := c.models.ListInferenceProfiles(ctx, &awsbedrock.ListInferenceProfilesInput{
			NextToken: next,
		})
		if err != nil {
			return nil, fmt.Errorf("bedrock: list inference profiles in %s: %w", c.region, err)
		}

		for _, p := range out.InferenceProfileSummaries {
			if p.InferenceProfileId == nil || p.Status != bedrocktypes.InferenceProfileStatusActive {
				continue
			}
			id := *p.InferenceProfileId

			model, regions := profileTargets(p.Models)
			if model == "" {
				continue
			}
			profiles = append(profiles, inferenceProfile{
				id:                id,
				foundationModelID: model,
				geography:         geographyOf(id),
				regions:           regions,
			})
		}

		if out.NextToken == nil || *out.NextToken == "" {
			return profiles, nil
		}
		next = out.NextToken
	}
}

// profileTargets reads the foundation model and the regions out of a profile's
// model ARNs.
//
// From the ARNs rather than the description, which reads "Routes requests to
// Claude Haiku 4.5 in ap-southeast-2, ap-southeast-4." That sentence is the
// same information in prose, and parsing prose for a compliance answer means
// the answer changes the day AWS rewords it. The ARN's region field is
// structural.
//
// A global profile carries an ARN with an empty region - arn:aws:bedrock:::… -
// which is how "anywhere" arrives, and it is left as an empty region list
// rather than turned into a guess about which regions that currently means.
func profileTargets(models []bedrocktypes.InferenceProfileModel) (string, []string) {
	var (
		model   string
		regions []string
	)
	for _, m := range models {
		if m.ModelArn == nil {
			continue
		}
		region, id := parseModelARN(*m.ModelArn)
		if id == "" {
			continue
		}
		if model == "" {
			model = id
		}
		if region != "" && !slices.Contains(regions, region) {
			regions = append(regions, region)
		}
	}
	slices.Sort(regions)
	return model, regions
}

// parseModelARN pulls the region and the model id out of a foundation-model
// ARN: arn:aws:bedrock:ap-southeast-2::foundation-model/anthropic.claude-…
func parseModelARN(arn string) (region, modelID string) {
	if slash := strings.LastIndex(arn, "/"); slash >= 0 {
		modelID = arn[slash+1:]
	}
	// arn : partition : service : region : account : resource
	if parts := strings.SplitN(arn, ":", 6); len(parts) >= 4 {
		region = parts[3]
	}
	return region, modelID
}

// geographyOf returns the inference-profile prefix as itself.
//
// An allowlist, matching the one pricing uses to decide which rate set applies,
// because the two must agree: a prefix this recognises as a geography but
// pricing does not would put a model in the console priced at nothing.
func geographyOf(profileID string) string {
	prefix, rest, found := strings.Cut(profileID, ".")
	if !found || rest == "" {
		return ""
	}
	if _, ok := knownGeographies[prefix]; ok {
		return prefix
	}
	return ""
}

// knownGeographies are the inference-profile prefixes AWS publishes, kept in
// step with pricing's own list.
var knownGeographies = map[string]struct{}{
	"global": {}, "us": {}, "us-gov": {}, "eu": {}, "apac": {}, "jp": {},
	"au": {}, "ca": {}, "sa": {}, "in": {}, "il": {}, "mx": {}, "kr": {},
}

// ModelAccess asks Bedrock what it says about one model's availability to this
// account: GetFoundationModelAvailability, for the foundation model behind an
// inference profile.
//
// On the catalogue rather than the invoking provider because it is a
// control-plane call, on the client discovery already holds. Evidence beside
// a refusal and nothing more - see providers.ModelAccess. Checked against the
// live API: every Anthropic model in an account reads agreement NOT_AVAILABLE
// until it is first invoked, and a model refused as "not available for this
// account" still reads AUTHORIZED and AVAILABLE throughout.
func (c *Catalogue) ModelAccess(ctx context.Context, model string) (providers.ModelAccess, error) {
	id := model
	if g := geographyOf(id); g != "" {
		id = strings.TrimPrefix(id, g+".")
	}
	out, err := c.models.GetFoundationModelAvailability(ctx, &awsbedrock.GetFoundationModelAvailabilityInput{
		ModelId: &id,
	})
	if err != nil {
		return providers.ModelAccess{}, fmt.Errorf("bedrock: model availability for %s in %s: %w", id, c.region, err)
	}
	access := providers.ModelAccess{
		Model:         id,
		Authorization: string(out.AuthorizationStatus),
		Entitlement:   string(out.EntitlementAvailability),
		Region:        string(out.RegionAvailability),
	}
	if a := out.AgreementAvailability; a != nil {
		access.Agreement = string(a.Status)
		access.AgreementError = derefString(a.ErrorMessage)
	}
	return access, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
