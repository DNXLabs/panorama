// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dnxlabs/panorama/internal/providers"
)

// RuntimeRegions are the regions bedrock-runtime is offered in, for a picker.
//
// A picker's list and nothing more: a region absent from it is still accepted
// if it is shaped like one, because whether Bedrock is really there is what the
// live check answers, and a list compiled into a release is out of date the day
// AWS opens a region.
var RuntimeRegions = []string{
	"us-east-1", "us-east-2", "us-west-2", "us-gov-west-1", "us-gov-east-1",
	"ca-central-1", "sa-east-1", "mx-central-1",
	"eu-central-1", "eu-central-2", "eu-west-1", "eu-west-2", "eu-west-3",
	"eu-north-1", "eu-south-1", "eu-south-2",
	"ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2",
	"ap-southeast-3", "ap-southeast-4", "ap-southeast-5", "ap-southeast-7",
	"ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
	"me-central-1", "il-central-1",
}

// MantleRegions are the regions bedrock-mantle is offered in, from AWS's
// bedrock-mantle page. Fourteen, and noticeably fewer than
// runtime: several model cards list regions (ap-southeast-4, for one) that
// runtime serves and Mantle does not.
var MantleRegions = []string{
	"us-east-1", "us-east-2", "us-west-2", "us-gov-west-1",
	"sa-east-1",
	"eu-central-1", "eu-west-1", "eu-west-2", "eu-south-1", "eu-north-1",
	"ap-south-1", "ap-southeast-2", "ap-southeast-3", "ap-northeast-1",
}

// Descriptors returns what cmd/panorama registers for the two Bedrock kinds.
func Descriptors() []providers.Descriptor {
	policy := TaskRolePolicyJSON()
	return []providers.Descriptor{
		{
			Kind:  providers.KindBedrock,
			Label: "Amazon Bedrock",
			Summary: "bedrock-runtime. Claude through inference profiles, so a geography - us., eu., " +
				"au., global. - decides which countries a request may be processed in and what it " +
				"costs. Serves the Anthropic Messages API.",
			Regions:         RuntimeRegions,
			ValidateOptions: validateRuntimeOptions,
			Build:           buildRuntime,
			Hint:            runtimeHint,
			ModelSpecific:   runtimeModelSpecific,
			Policy:          policy,
			Endpoint:        func(region string) string { return "bedrock-runtime." + region + ".amazonaws.com" },
		},
		{
			Kind:  providers.KindBedrockMantle,
			Label: "Amazon Bedrock (Mantle)",
			Summary: "bedrock-mantle. Every model invoked in the region you choose, with no inference " +
				"profile - Claude over the Anthropic Messages API, and open-weight and other models " +
				"that are served nowhere else over the OpenAI Chat Completions and Responses APIs.",
			Regions:  MantleRegions,
			Build:    buildMantle,
			Hint:     mantleHint,
			Policy:   policy,
			Endpoint: MantleHost,
		},
	}
}

// validateRuntimeOptions refuses an API override the runtime provider cannot
// serve. It sends the Anthropic Messages body through InvokeModel and nothing
// else, so a model marked as speaking Chat Completions here would be a route
// that fails on its first request.
func validateRuntimeOptions(o providers.Options) error {
	for key, list := range o.APIs {
		for _, a := range list {
			if a != providers.APIAnthropicMessages {
				return fmt.Errorf("%s: the %s provider serves the Anthropic Messages API only, so %s cannot be "+
					"switched on for a model here; enable bedrock-mantle for it instead", key, Name, a)
			}
		}
	}
	return nil
}

func buildRuntime(ctx context.Context, s providers.Settings, log *slog.Logger) (*providers.Instance, error) {
	conf := ConfigFrom(s, log)
	provider, err := New(ctx, conf)
	if err != nil {
		return nil, err
	}
	catalogue, err := NewCatalogue(ctx, conf)
	if err != nil {
		return nil, err
	}
	prices, err := NewPriceSource(ctx, conf)
	if err != nil {
		return nil, err
	}
	return &providers.Instance{
		Provider:  provider,
		Catalogue: catalogue,
		Prices:    prices,
		APIs:      catalogue.apis,
	}, nil
}

// runtimeAPIs applies an operator's override - which can only narrow a model to
// browse-only, or restate Messages - over RuntimeAPIs.
func runtimeAPIs(overrides map[string][]providers.API) func(string) []providers.API {
	return func(model string) []providers.API {
		if list, ok := overrideFor(overrides, model); ok {
			return append([]providers.API(nil), list...)
		}
		return RuntimeAPIs(model)
	}
}

func buildMantle(ctx context.Context, s providers.Settings, log *slog.Logger) (*providers.Instance, error) {
	conf := ConfigFrom(s, log)
	provider, err := NewMantle(ctx, conf)
	if err != nil {
		return nil, err
	}
	catalogue := NewMantleCatalogue(provider, conf.ContextWindows)
	prices, err := NewMantlePriceSource(ctx, conf, catalogue)
	if err != nil {
		return nil, err
	}
	return &providers.Instance{
		Provider:  provider,
		Catalogue: catalogue,
		Prices:    prices,
		APIs:      provider.apis,
	}, nil
}

// runtimeHint turns the failures everybody hits into the fix.
//
// Matched on the provider's own text rather than on a classified error,
// because Bedrock reports causes with nothing in common under one exception:
// an account that has not submitted Anthropic's use case form and a model that
// has been retired are both ResourceNotFoundException, and a missing IAM
// action, an SCP, a Marketplace subscription and a model AWS does not offer
// this account are all AccessDeniedException. Each needs somebody to open a
// different page, so each is told apart by the sentence AWS wrote, quoted here
// as observed against the live API.
func runtimeHint(message string) string {
	lower := strings.ToLower(message)
	if hint, ok := credentialHint(lower); ok {
		return hint
	}
	switch {
	case strings.Contains(lower, "use case details have not been submitted"):
		// "Model use case details have not been submitted for this account.
		// Fill out the Anthropic use case details form before using the
		// model." Once per AWS account, for every Anthropic model at once.
		return "Anthropic asks every AWS account to describe its use case once before any of its models can be invoked, and this account has not. In the Amazon Bedrock console, open any Anthropic model in the Model catalog and submit the use case details it asks for. It applies to every Anthropic model, and can take up to 15 minutes to take effect; check again after that. Nothing in Panorama's role or configuration changes."
	case strings.Contains(lower, "is not available for this account"):
		// "anthropic.claude-haiku-5-5 is not available for this account. You
		// can explore other available models on Amazon Bedrock."
		return "AWS does not offer this model to this AWS account. This is decided by AWS per account and is not a permission: no IAM change grants it. Choose another model - Refresh the catalogue and pick one - or ask your AWS account team for access to this one."
	case strings.Contains(lower, "end of its life"), strings.Contains(lower, "end-of-life"),
		strings.Contains(lower, "legacy"), strings.Contains(lower, "no longer available"):
		return "Bedrock has retired this model, or this version of it. Point the route at a current model from the catalogue."
	case strings.Contains(lower, "explicit deny"), strings.Contains(lower, "service control policy"),
		strings.Contains(lower, "permissions boundary"):
		// The role is fine and something above it says no. Granting the role
		// more cannot fix it, so saying "add bedrock:InvokeModel" here would
		// send somebody round in a circle.
		return "A policy outside Panorama's role denies this: a service control policy on the account or its organizational unit, or a permissions boundary on the role. Granting the role more cannot override an explicit deny; whoever administers the AWS organization has to allow Bedrock for this account, region and model."
	case strings.Contains(lower, "aws-marketplace"), strings.Contains(lower, "marketplace"),
		strings.Contains(lower, "don't have access"), strings.Contains(lower, "do not have access"):
		// Bedrock subscribes the account to a model through AWS Marketplace on
		// its first invocation. That needs the two marketplace actions, and it
		// is not instant: until the subscription is active the same request
		// is refused.
		return "Bedrock subscribes this AWS account to a model through AWS Marketplace on its first invocation, and the subscription is not active yet. If Panorama's role has aws-marketplace:Subscribe and aws-marketplace:ViewSubscriptions (the policy below has both), this usually clears within a few minutes: check again shortly. If it has not cleared after 15 minutes, look in AWS Marketplace, Manage subscriptions, for a failed or pending agreement for the model, and check that no service control policy denies aws-marketplace:Subscribe."
	case strings.Contains(lower, "not authorized to perform"), strings.Contains(lower, "accessdenied"),
		strings.Contains(lower, "access denied"), strings.Contains(lower, "not authorized"):
		return "The credentials Panorama is running with are not permitted to invoke this model. They need bedrock:InvokeModel and bedrock:InvokeModelWithResponseStream on it, and aws-marketplace:Subscribe with aws-marketplace:ViewSubscriptions on * - Bedrock subscribes the account to a model on the first invocation, and without those the first call fails like this. The policy below is the whole of what the role needs."
	case strings.Contains(lower, "modelnotready"):
		return "Bedrock is still preparing this model in this account. Check again in a few minutes."
	case strings.Contains(lower, "validation"), strings.Contains(lower, "invalid"), strings.Contains(lower, "not found"),
		strings.Contains(lower, "notfound"):
		return "Bedrock did not recognise this identifier. Current Anthropic models require an inference profile - the us. or global. prefixed form. Refresh the catalogue and choose one from it."
	case strings.Contains(lower, "could not resolve"), strings.Contains(lower, "no such host"), strings.Contains(lower, "timeout"):
		return "Panorama could not reach Bedrock. Check the region and that this deployment has egress to bedrock-runtime in it."
	default:
		return "Check the region, the model, and that the deployment's credentials can invoke it."
	}
}

// runtimeModelSpecific reports the refusals that are about one model and not
// about the deployment: a model AWS does not offer this account, and a retired
// one. Everything else - credentials, IAM, an SCP, Anthropic's form, which
// covers every Anthropic model at once - fails the next model the same way.
func runtimeModelSpecific(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "is not available for this account") ||
		strings.Contains(lower, "end of its life") || strings.Contains(lower, "end-of-life")
}

// credentialHint names the two failures that are about having credentials at
// all, as opposed to what they are allowed to do, and both providers ask it
// first. Without it they were read as something else: "the security token ...
// is invalid" matched the wrong-identifier hint on "invalid", and "get
// credentials: ..." matched bedrock-mantle's IAM hint on "credentials", so an
// operator with no role attached was told which actions to grant it.
//
// It is the first thing a laptop meets, because a check that finds no
// catalogue now reads one, and temporary credentials exported into a
// container's environment stop working within hours.
func credentialHint(lower string) (string, bool) {
	switch {
	case strings.Contains(lower, "expiredtoken"), strings.Contains(lower, "token included in the request is expired"),
		strings.Contains(lower, "unrecognizedclient"), strings.Contains(lower, "invalidclienttokenid"),
		strings.Contains(lower, "token included in the request is invalid"):
		return "The AWS credentials Panorama is running with have expired or are not valid. Temporary credentials in a container's environment do not renew themselves: export fresh ones and start the container again. A role attached to the instance or the task does not have this problem.", true
	case strings.Contains(lower, "get credentials"), strings.Contains(lower, "failed to refresh cached credentials"),
		strings.Contains(lower, "no ec2 imds role found"):
		return "Panorama found no AWS credentials. It uses the AWS SDK's default chain and has no field for a key: on a server, attach an instance role or a task role; on a laptop, put credentials in the environment the container starts with, and start it again.", true
	}
	return "", false
}

// mantleHint is the same, for the failures bedrock-mantle produces.
func mantleHint(message string) string {
	lower := strings.ToLower(message)
	if hint, ok := credentialHint(lower); ok {
		return hint
	}
	switch {
	case strings.Contains(lower, "403"), strings.Contains(lower, "401"), strings.Contains(lower, "accessdenied"),
		strings.Contains(lower, "not authorized"), strings.Contains(lower, "credentials"):
		// The one everybody hits first: a role that can invoke bedrock-runtime
		// perfectly and has never been granted the other service's actions.
		return "The credentials Panorama is running with cannot call bedrock-mantle. It is a separate service with its own IAM actions - bedrock:InvokeModel does not cover it - so the role needs bedrock-mantle:CreateInference on arn:aws:bedrock-mantle:*:*:project/* and bedrock-mantle:ListModels. The policy below is the whole of what the role needs."
	case strings.Contains(lower, "404"), strings.Contains(lower, "not found"):
		return "bedrock-mantle does not serve this model in this region. Refresh the catalogue for the region, and check the model's availability there."
	case strings.Contains(lower, "could not resolve"), strings.Contains(lower, "no such host"), strings.Contains(lower, "timeout"),
		strings.Contains(lower, "could not be reached"):
		return "Panorama could not reach bedrock-mantle. Check that the region offers it and that this deployment has egress to bedrock-mantle.<region>.api.aws, or a com.amazonaws.<region>.bedrock-mantle VPC endpoint."
	case strings.Contains(lower, "429"), strings.Contains(lower, "rate limited"):
		return "bedrock-mantle is throttling this account. Its quotas are separate from bedrock-runtime's, and are raised in Service Quotas under Amazon Bedrock."
	default:
		return "Check the region, the model, and that the deployment's credentials can call bedrock-mantle."
	}
}
