// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package fakebedrock

import (
	"net/http"
	"strconv"
	"time"
)

// The fixture region. It appears in every ARN, and profileTargets reads the
// region out of the ARN rather than out of a profile's description, so this is
// what a discovered row reports as its residency.
const fixtureRegion = "us-east-1"

// CatalogueModel is one row of ListFoundationModels.
//
// The field names are the wire's, so a fixture reads the same way as a captured
// response and a test can build one without a translation step.
type CatalogueModel struct {
	ModelArn                   string   `json:"modelArn"`
	ModelID                    string   `json:"modelId"`
	ModelName                  string   `json:"modelName"`
	ProviderName               string   `json:"providerName"`
	InputModalities            []string `json:"inputModalities"`
	OutputModalities           []string `json:"outputModalities"`
	ResponseStreamingSupported bool     `json:"responseStreamingSupported"`
	InferenceTypesSupported    []string `json:"inferenceTypesSupported"`
	ModelLifecycle             struct {
		Status string `json:"status"`
	} `json:"modelLifecycle"`
}

// CatalogueProfile is one row of ListInferenceProfiles.
type CatalogueProfile struct {
	InferenceProfileArn  string `json:"inferenceProfileArn"`
	InferenceProfileID   string `json:"inferenceProfileId"`
	InferenceProfileName string `json:"inferenceProfileName"`
	Description          string `json:"description"`
	Status               string `json:"status"`
	Type                 string `json:"type"`
	Models               []struct {
		ModelArn string `json:"modelArn"`
	} `json:"models"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func modelARN(region, id string) string {
	return "arn:aws:bedrock:" + region + "::foundation-model/" + id
}

// model builds a fixture row.
//
// requiresProfile is the interesting flag: AWS reports INFERENCE_PROFILE for
// almost every current Anthropic model, which is the machine-readable form of
// the rule an operator otherwise learns by a request failing. The fixture keeps
// that proportion so discovery's handling of it is exercised rather than
// assumed.
func model(id, name string, requiresProfile bool, lifecycle string) CatalogueModel {
	m := CatalogueModel{
		ModelArn:                   modelARN(fixtureRegion, id),
		ModelID:                    id,
		ModelName:                  name,
		ProviderName:               "Anthropic",
		InputModalities:            []string{"TEXT", "IMAGE"},
		OutputModalities:           []string{"TEXT"},
		ResponseStreamingSupported: true,
	}
	if requiresProfile {
		m.InferenceTypesSupported = []string{"INFERENCE_PROFILE"}
	} else {
		m.InferenceTypesSupported = []string{"ON_DEMAND"}
	}
	m.ModelLifecycle.Status = lifecycle
	return m
}

func profile(id, name, foundation string, regions ...string) CatalogueProfile {
	p := CatalogueProfile{
		InferenceProfileArn:  "arn:aws:bedrock:" + fixtureRegion + ":000000000000:inference-profile/" + id,
		InferenceProfileID:   id,
		InferenceProfileName: name,
		Description:          "Routes requests to " + foundation + ".",
		Status:               "ACTIVE",
		Type:                 "SYSTEM_DEFINED",
		CreatedAt:            time.Unix(0, 0).UTC().Format(time.RFC3339),
		UpdatedAt:            time.Unix(0, 0).UTC().Format(time.RFC3339),
	}
	for _, r := range regions {
		p.Models = append(p.Models, struct {
			ModelArn string `json:"modelArn"`
		}{ModelArn: modelARN(r, foundation)})
	}
	return p
}

// The foundation model ids the fixtures are built on.
//
// They are real ids rather than invented ones, and they are ids pricing.Seed()
// carries, because a route pointing at a model with no rate records a null cost
// - correct behaviour, and useless for a test that wants to assert what a
// request cost.
const (
	FoundationSonnet5 = "anthropic.claude-sonnet-5"
	FoundationHaiku45 = "anthropic.claude-haiku-4-5-20251001-v1:0"
	FoundationOpus5   = "anthropic.claude-opus-5"
	FoundationLegacy  = "anthropic.claude-3-haiku-20240307-v1:0"
)

// The profile ids a route may target.
//
// us. and global. are two entries for one model on purpose: they route to
// different countries and bill about 10% apart, and allowing one while refusing
// the other is the whole point of a geography being a row rather than a flag.
const (
	TargetSonnet5US     = "us." + FoundationSonnet5
	TargetSonnet5Global = "global." + FoundationSonnet5
	TargetHaiku45US     = "us." + FoundationHaiku45
	TargetHaiku45Global = "global." + FoundationHaiku45
	TargetOpus5US       = "us." + FoundationOpus5
)

// DefaultModels is the foundation-model catalogue discovery reports.
func DefaultModels() []CatalogueModel {
	return []CatalogueModel{
		model(FoundationSonnet5, "Claude Sonnet 5", true, "ACTIVE"),
		model(FoundationHaiku45, "Claude Haiku 4.5", true, "ACTIVE"),
		model(FoundationOpus5, "Claude Opus 5", true, "ACTIVE"),
		// The one model still invocable by its bare id, and it is LEGACY -
		// which is the shape us-east-1 is actually in, and the reason a bare-id
		// row sorts last and is labelled "no profile" rather than led with.
		model(FoundationLegacy, "Claude 3 Haiku", false, "LEGACY"),
	}
}

// DefaultProfiles is the inference-profile catalogue discovery reports.
func DefaultProfiles() []CatalogueProfile {
	return []CatalogueProfile{
		profile(TargetSonnet5US, "US Claude Sonnet 5", FoundationSonnet5,
			"us-east-1", "us-east-2", "us-west-2"),
		profile(TargetSonnet5Global, "Global Claude Sonnet 5", FoundationSonnet5,
			"us-east-1", "eu-west-1", "ap-southeast-2"),
		profile(TargetHaiku45US, "US Claude Haiku 4.5", FoundationHaiku45,
			"us-east-1", "us-west-2"),
		profile(TargetHaiku45Global, "Global Claude Haiku 4.5", FoundationHaiku45,
			"us-east-1", "eu-west-1", "ap-southeast-2"),
		profile(TargetOpus5US, "US Claude Opus 5", FoundationOpus5,
			"us-east-1", "us-west-2"),
	}
}

func (s *Server) catalogue() ([]CatalogueModel, []CatalogueProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models, s.profiles
}

func (s *Server) handleFoundationModels(w http.ResponseWriter, _ *http.Request) {
	models, _ := s.catalogue()
	writeJSON(w, map[string]any{"modelSummaries": models})
}

// profilePageSize is small so a default catalogue spans several pages.
//
// ListInferenceProfiles is the one call this product pages through by hand, and
// a fixture that fitted in one response would leave that loop untested - the
// failure mode being a deployment that silently discovers only the first page.
const profilePageSize = 2

func (s *Server) handleInferenceProfiles(w http.ResponseWriter, r *http.Request) {
	_, profiles := s.catalogue()

	start := 0
	if tok := r.URL.Query().Get("nextToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 || n > len(profiles) {
			writeAWSError(w, http.StatusBadRequest, "ValidationException",
				"fakebedrock: bad nextToken "+tok)
			return
		}
		start = n
	}

	end := min(start+profilePageSize, len(profiles))

	page := map[string]any{"inferenceProfileSummaries": profiles[start:end]}
	if end < len(profiles) {
		page["nextToken"] = strconv.Itoa(end)
	}
	writeJSON(w, page)
}
