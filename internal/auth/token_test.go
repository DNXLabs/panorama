// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateProducesDistinctTokens(t *testing.T) {
	seen := make(map[string]bool, 200)

	for range 200 {
		tok, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[tok.Plaintext] {
			t.Fatal("Generate returned a duplicate token")
		}
		seen[tok.Plaintext] = true

		if !strings.HasPrefix(tok.Plaintext, Prefix) {
			t.Errorf("token %q lacks the %q prefix that makes a leak recognisable", tok.Plaintext, Prefix)
		}
		if tok.Hash == tok.Plaintext {
			t.Fatal("the stored hash equals the token; the secret would be stored in the clear")
		}
		if strings.Contains(tok.Hash, strings.TrimPrefix(tok.Plaintext, Prefix)) {
			t.Fatal("the hash contains the secret")
		}
		if !strings.HasPrefix(tok.Plaintext, tok.DisplayPrefix) {
			t.Errorf("DisplayPrefix %q is not a prefix of the token", tok.DisplayPrefix)
		}
		if len(tok.DisplayPrefix) >= len(tok.Plaintext) {
			t.Error("DisplayPrefix reveals the whole token")
		}
	}
}

func TestHashIsStable(t *testing.T) {
	const sample = "pnt_example"
	first, second := Hash(sample), Hash(sample)
	if first != second {
		t.Fatal("Hash is not deterministic")
	}
	if Hash("pnt_a") == Hash("pnt_b") {
		t.Fatal("Hash collided on different inputs")
	}
	if got := len(first); got != 64 {
		t.Errorf("hash length = %d, want 64 hex characters for SHA-256", got)
	}
}

func TestParse(t *testing.T) {
	valid, err := Generate()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		authorization string
		apiKey        string
		want          string
		wantErr       bool
	}{
		{
			name:          "bearer header, as ANTHROPIC_AUTH_TOKEN sends it",
			authorization: "Bearer " + valid.Plaintext,
			want:          valid.Plaintext,
		},
		{
			name:          "bearer scheme is case-insensitive",
			authorization: "bearer " + valid.Plaintext,
			want:          valid.Plaintext,
		},
		{
			name:   "x-api-key, as ANTHROPIC_API_KEY sends it",
			apiKey: valid.Plaintext,
			want:   valid.Plaintext,
		},
		{
			name:          "authorization wins when both are present",
			authorization: "Bearer " + valid.Plaintext,
			apiKey:        "pnt_somethingelse",
			want:          valid.Plaintext,
		},
		{
			name:    "nothing supplied",
			wantErr: true,
		},
		{
			name:          "wrong scheme",
			authorization: "Basic " + valid.Plaintext,
			wantErr:       true,
		},
		{
			name:          "no scheme",
			authorization: valid.Plaintext,
			wantErr:       true,
		},
		{
			// A real Anthropic key must never be hashed, looked up, or
			// forwarded. Rejecting on shape keeps it out of the system.
			name:    "a real anthropic key is refused on shape",
			apiKey:  "sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			wantErr: true,
		},
		{
			name:    "right prefix, wrong length",
			apiKey:  Prefix + "tooshort",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.authorization, tt.apiKey)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got token %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got != tt.want {
				t.Errorf("token = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseAcceptsSpendableCredentialsOnly is the boundary this package exists
// to hold. A personal token and a service token may buy model calls; a CLI
// grant is long-lived and may only be exchanged, so the gateway must refuse it
// even though it is a credential Panorama itself issued.
func TestParseAcceptsSpendableCredentialsOnly(t *testing.T) {
	user, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	service, err := GenerateService()
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	grant, err := GenerateGrant()
	if err != nil {
		t.Fatalf("GenerateGrant: %v", err)
	}

	for _, tc := range []struct {
		name    string
		token   string
		allowed bool
	}{
		{"personal token", user.Plaintext, true},
		{"service token", service.Plaintext, true},
		{"cli grant", grant.Plaintext, false},
		{"console session", SessionPrefix + strings.TrimPrefix(user.Plaintext, Prefix), false},
		{"setup session", SetupSessionPrefix + strings.TrimPrefix(user.Plaintext, Prefix), false},
		{"real anthropic key", "sk-ant-api03-" + strings.Repeat("x", 40), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, via := range []struct {
				how           string
				authorization string
				apiKey        string
			}{
				{"bearer", "Bearer " + tc.token, ""},
				{"x-api-key", "", tc.token},
			} {
				got, err := Parse(via.authorization, via.apiKey)
				if tc.allowed {
					if err != nil {
						t.Errorf("Parse via %s: %v, want the token accepted", via.how, err)
					} else if got != tc.token {
						t.Errorf("Parse via %s = %q, want %q", via.how, got, tc.token)
					}
					continue
				}
				if err == nil {
					t.Errorf("Parse via %s accepted %s; a credential that cannot buy model calls "+
						"must be refused before it is hashed or looked up", via.how, tc.name)
				}
			}
		})
	}
}

func TestParseGrantAcceptsOnlyGrants(t *testing.T) {
	grant, err := GenerateGrant()
	if err != nil {
		t.Fatalf("GenerateGrant: %v", err)
	}
	user, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if got, err := ParseGrant("Bearer " + grant.Plaintext); err != nil || got != grant.Plaintext {
		t.Errorf("ParseGrant(grant) = %q, %v; want the grant accepted", got, err)
	}
	if _, err := ParseGrant("Bearer " + user.Plaintext); err == nil {
		t.Error("ParseGrant accepted a gateway token; the exchange endpoint must not take one")
	}
	if _, err := ParseGrant(grant.Plaintext); err == nil {
		t.Error("ParseGrant accepted a bare value with no Bearer scheme")
	}
}

func TestKindOfNamesTheTableToLookIn(t *testing.T) {
	user, _ := Generate()
	service, _ := GenerateService()
	grant, _ := GenerateGrant()

	if got := KindOf(user.Plaintext); got != KindUser {
		t.Errorf("KindOf(personal) = %q, want %q", got, KindUser)
	}
	if got := KindOf(service.Plaintext); got != KindService {
		t.Errorf("KindOf(service) = %q, want %q", got, KindService)
	}
	if got := KindOf(grant.Plaintext); got != "" {
		t.Errorf("KindOf(grant) = %q, want %q - a grant names no gateway table", got, "")
	}
}

// TestNewUserCodeAvoidsCharactersReadWrong guards the one credential in the
// product that is transcribed by eye rather than pasted.
func TestNewUserCodeAvoidsCharactersReadWrong(t *testing.T) {
	seen := make(map[rune]bool)
	codes := make(map[string]bool, 500)

	for range 500 {
		code, err := NewUserCode()
		if err != nil {
			t.Fatalf("NewUserCode: %v", err)
		}
		if codes[code] {
			t.Fatal("NewUserCode returned a duplicate within 500 draws")
		}
		codes[code] = true

		if len(code) != userCodeLength+1 || code[userCodeLength/2] != '-' {
			t.Fatalf("NewUserCode() = %q, want eight characters split by a dash", code)
		}
		for _, r := range code {
			if r == '-' {
				continue
			}
			if !strings.ContainsRune(userCodeAlphabet, r) {
				t.Errorf("code %q contains %q, which is not in the alphabet", code, r)
			}
			seen[r] = true
		}
	}

	// Rejection sampling should reach the whole alphabet in 500 draws of eight
	// characters; a modulo shortcut would too, so this is not what catches
	// bias - it catches an alphabet or a limit that excludes its own tail.
	for _, r := range userCodeAlphabet {
		if !seen[r] {
			t.Errorf("character %q never appeared in 4000 draws; the sampler cannot reach it", r)
		}
	}
	for _, r := range "ILOU01" {
		if strings.ContainsRune(userCodeAlphabet, r) {
			t.Errorf("alphabet contains %q, which is misread when transcribed", r)
		}
	}
}

// TestAGatewayTokenLivesThirtyDaysAndOnlyShortens. A pasted token does not
// rotate and nothing about a lost laptop tells the deployment to revoke it, so
// the expiry is the one guarantee available without asking anybody to remember
// something. An operator may say "shorter"; nobody may say "longer".
func TestAGatewayTokenLivesThirtyDaysAndOnlyShortens(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	if got := GatewayTokenExpiry(now, 0); !got.Equal(now.AddDate(0, 0, 30)) {
		t.Errorf("with no ceiling, expiry = %s, want 30 days out", got)
	}

	// A deployment that has said a week means a week.
	week := 7 * 24 * time.Hour
	if got := GatewayTokenExpiry(now, week); !got.Equal(now.Add(week)) {
		t.Errorf("with a 7 day ceiling, expiry = %s, want the ceiling", got)
	}
	if got := GatewayTokenLifetime(week); got != week {
		t.Errorf("lifetime = %s, want the deployment's own ceiling", got)
	}

	// And a ceiling above thirty days does not raise it. There is no setting
	// that hands out a longer gateway token, which is what "always" means.
	if got := GatewayTokenExpiry(now, 365*24*time.Hour); !got.Equal(now.AddDate(0, 0, 30)) {
		t.Errorf("with a year's ceiling, expiry = %s, want 30 days out anyway", got)
	}
}
