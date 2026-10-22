// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import "encoding/json"

// The permissions a deployment's role needs, as one value.
//
// This value is the one copy: the console and `panorama providers policy`
// render it, the README points at the template, and
// TestTaskRolePolicyMatchesTheTemplate fails the build the day the template and
// this value disagree. A policy that is out of step in one place is found by an
// operator whose first request fails AccessDenied.
//
// It grants both Bedrock endpoints in every region, and that is the design
// rather than a convenience. Which region each provider invokes is chosen in
// the console, by an administrator, long after the role was created; a policy
// scoped to one region would turn that choice into an IAM change nobody holding
// the console can make, and the failure would be an AccessDenied that looks
// exactly like a model that was never enabled.

// PolicyDocument is an IAM policy in the shape AWS reads.
type PolicyDocument struct {
	Version   string            `json:"Version"`
	Statement []PolicyStatement `json:"Statement"`
}

// PolicyStatement is one statement of it.
type PolicyStatement struct {
	Sid      string   `json:"Sid"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// TaskRolePolicy is what the deployment's role must allow.
//
// Five statements, and the reasons for the ones people get wrong are recorded
// beside each rather than in a document somebody has to find.
var TaskRolePolicy = PolicyDocument{
	Version: "2012-10-17",
	Statement: []PolicyStatement{
		{
			// Two ARN shapes because one invocation is checked against both.
			// Current Anthropic models refuse their bare model id and are reached
			// through an inference profile, and AWS authorises that against the
			// profile *and* against the foundation model in every region the
			// profile may route to. A global. profile's foundation-model ARN has
			// an empty region field, which the wildcard matches and a
			// hand-written region does not.
			Sid:    "InvokeThroughInferenceProfiles",
			Effect: "Allow",
			Action: []string{
				"bedrock:InvokeModel",
				"bedrock:InvokeModelWithResponseStream",
			},
			Resource: []string{
				"arn:aws:bedrock:*::foundation-model/*",
				"arn:aws:bedrock:*:*:inference-profile/*",
			},
		},
		{
			// bedrock-mantle is a separate service with its own action prefix,
			// and bedrock:InvokeModel does not authorise it. Every inference call
			// - Anthropic Messages, Chat Completions, Responses - is the one
			// CreateInference action, checked against the project the request
			// runs in; a request that names none runs in the account's default
			// project, which project/* covers.
			//
			// Not granted, deliberately: CallWithBearerToken (this gateway signs
			// with SigV4 and never holds a long-term Bedrock API key), data
			// retention and project administration, and the files and
			// fine-tuning actions. Nothing here conditions on
			// bedrock-mantle:Model, because which models are used is decided by
			// a route in the console, and a policy that also decided it would be
			// a second allowlist that disagrees with the first the day somebody
			// adds a route.
			Sid:    "InvokeThroughBedrockMantle",
			Effect: "Allow",
			Action: []string{
				"bedrock-mantle:CreateInference",
				"bedrock-mantle:CountTokens",
			},
			Resource: []string{
				"arn:aws:bedrock-mantle:*:*:project/*",
			},
		},
		{
			// Discovery and pricing, on both endpoints. Resource "*" because the
			// list actions are not scoped to a resource, and writing a narrower
			// one here would fail discovery in a way that reads as an empty
			// account rather than a policy mistake. GetFoundationModelAvailability
			// is the provider check's evidence beside a refusal - what Bedrock
			// says about the agreement, the authorization and the entitlement -
			// and is read, never acted on.
			Sid:    "DiscoverAndPriceModels",
			Effect: "Allow",
			Action: []string{
				"bedrock:ListFoundationModels",
				"bedrock:ListInferenceProfiles",
				"bedrock:GetFoundationModelAvailability",
				"bedrock-mantle:ListModels",
				"bedrock-mantle:GetModel",
				"pricing:GetProducts",
			},
			Resource: []string{"*"},
		},
		{
			// The statement people leave out, because nothing about it says
			// Bedrock. Bedrock subscribes the account to a foundation model on
			// its first invocation, so without this the first request against a
			// model this account has never used fails as AccessDenied - the same
			// error a missing InvokeModel gives. It cannot be scoped.
			Sid:    "SubscribeOnFirstInvocation",
			Effect: "Allow",
			Action: []string{
				"aws-marketplace:Subscribe",
				"aws-marketplace:ViewSubscriptions",
			},
			Resource: []string{"*"},
		},
		webSearchStatement,
	},
}

// webSearchStatement lets the deployment call an AgentCore gateway, which is
// how web search is reached. Any gateway in any region, for the reason the
// model statements are unscoped: which gateway is chosen in the console, after
// this role exists, and that choice must not need an IAM change.
//
// It is all the role needs. The search itself is authorised against the
// gateway's own service role (bedrock-agentcore:InvokeWebSearch), created with
// the gateway, so this role cannot search except through a gateway somebody
// configured.
var webSearchStatement = PolicyStatement{
	Sid:      "InvokeWebSearchGateway",
	Effect:   "Allow",
	Action:   []string{"bedrock-agentcore:InvokeGateway"},
	Resource: []string{"arn:aws:bedrock-agentcore:*:*:gateway/*"},
}

// WebSearchPolicy is the part of TaskRolePolicy that web search needs, for
// the settings screen to show beside the gateway field.
var WebSearchPolicy = PolicyDocument{
	Version:   "2012-10-17",
	Statement: []PolicyStatement{webSearchStatement},
}

// TaskRolePolicyJSON renders the policy the way an operator pastes it.
func TaskRolePolicyJSON() string {
	out, err := json.MarshalIndent(TaskRolePolicy, "", "  ")
	if err != nil {
		// A literal of strings cannot fail to encode. Panicking here is a
		// programming error surfacing at the first test run, not a runtime path.
		panic("bedrock: encoding the task role policy: " + err.Error())
	}
	return string(out)
}
