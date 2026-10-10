// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"strings"
	"testing"
)

// TestHavingNoCredentialsIsNotReadAsSomethingElse.
//
// The hints match on the provider's own text, and the two failures about
// having credentials at all must not land on hints for other faults: "the
// security token ... is invalid" would reach the wrong-identifier hint through
// "invalid", and "get credentials: ..." bedrock-mantle's IAM hint through
// "credentials", telling an operator with no role attached which actions to
// grant it. Each message here is one a deployment actually produced.
func TestHavingNoCredentialsIsNotReadAsSomethingElse(t *testing.T) {
	const (
		// A container with nothing in its environment and no role: what the
		// Quickstart produces when the credentials line was skipped.
		none = "jobs: discovering bedrock models in us-east-1: bedrock: list foundation models in us-east-1: " +
			"operation error Bedrock: ListFoundationModels, get identity: get credentials: failed to refresh " +
			"cached credentials, no EC2 IMDS role found, operation error ec2imds: GetMetadata, http response " +
			"error StatusCode: 403, request to EC2 IMDS failed"
		// Temporary credentials exported into .env that morning.
		expired = "operation error Bedrock Runtime: InvokeModel, https response error StatusCode: 403, " +
			"ExpiredTokenException: The security token included in the request is expired"
		// A key that was rotated or mistyped.
		invalid = "operation error Bedrock Runtime: InvokeModel, https response error StatusCode: 403, " +
			"UnrecognizedClientException: The security token included in the request is invalid."
	)

	for _, hint := range []func(string) string{runtimeHint, mantleHint} {
		if got := hint(none); !strings.Contains(got, "found no AWS credentials") {
			t.Errorf("no credentials was hinted as %q", got)
		}
		for _, message := range []string{expired, invalid} {
			if got := hint(message); !strings.Contains(got, "expired or are not valid") {
				t.Errorf("%q was hinted as %q", message, got)
			}
		}
	}

	// And it takes nothing from the faults the tables already named: a role
	// that exists and may not invoke is still an IAM problem, and an identifier
	// Bedrock does not recognise is still one.
	denied := "AccessDeniedException: User: arn:aws:sts::111122223333:assumed-role/panorama is not authorized " +
		"to perform: bedrock:InvokeModel"
	if got := runtimeHint(denied); !strings.Contains(got, "aws-marketplace:Subscribe") {
		t.Errorf("an access denial was hinted as %q", got)
	}
	if got := mantleHint(denied); !strings.Contains(got, "bedrock-mantle:CreateInference") {
		t.Errorf("an access denial on bedrock-mantle was hinted as %q", got)
	}
	unknown := "ValidationException: The provided model identifier is invalid."
	if got := runtimeHint(unknown); !strings.Contains(got, "inference profile") {
		t.Errorf("an unrecognised identifier was hinted as %q", got)
	}
}
