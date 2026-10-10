// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/secrets"
	"github.com/dnxlabs/panorama/internal/store"
)

const testPrivateKey = "-----BEGIN RSA PRIVATE KEY-----\nnot-a-real-key\n-----END RSA PRIVATE KEY-----"

// TestConnectingStoresTheCredentialEncrypted is the property that makes putting
// a credential in a database defensible at all.
func TestConnectingStoresTheCredentialEncrypted(t *testing.T) {
	key, err := secrets.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	f := newConsole(t, func(o *ConsoleOptions) {
		box, berr := secrets.NewBox(key)
		if berr != nil {
			t.Fatal(berr)
		}
		o.Secrets = box
	})

	rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/github", f.adminToken, map[string]any{
		"provider": "github", "token": "ghp_a_real_looking_token",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("connecting = %d: %s", rec.Code, rec.Body.String())
	}

	// What landed in the database is not the token.
	stored, err := f.db.IntegrationSecret(t.Context(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if stored == "" {
		t.Fatal("nothing was stored")
	}
	if strings.Contains(stored, "ghp_a_real_looking_token") {
		t.Error("the credential is in the database in the clear")
	}

	// And it round-trips with the key.
	box, _ := secrets.NewBox(key)
	opened, err := box.Open(stored)
	if err != nil {
		t.Fatalf("the stored credential cannot be read back: %v", err)
	}
	if opened != "ghp_a_real_looking_token" {
		t.Error("the stored credential did not round-trip")
	}
}

// TestTheCredentialNeverComesBackOut. A credential the console can read back is
// a credential the console can leak.
func TestTheCredentialNeverComesBackOut(t *testing.T) {
	key, _ := secrets.NewKey()
	f := newConsole(t, func(o *ConsoleOptions) {
		box, _ := secrets.NewBox(key)
		o.Secrets = box
	})

	const token = "ghp_a_real_looking_token"
	if rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/github", f.adminToken, map[string]any{
		"provider": "github", "token": token, "api_base_url": "https://ghe.example",
	}); rec.Code != http.StatusOK {
		t.Fatalf("connecting: %d", rec.Code)
	}

	rec, _ := f.get(t, "/api/v1/integrations", f.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("listing: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatal("the integrations endpoint returned the stored credential")
	}

	// It says one is stored, and what kind, without disclosing it.
	body := rec.Body.String()
	if !strings.Contains(body, `"has_secret":true`) {
		t.Error("the response does not report that a credential is stored")
	}
	if !strings.Contains(body, "https://ghe.example") {
		t.Error("the non-secret settings did not come back, so the form cannot re-open with them")
	}
}

// TestEditingAConnectionKeepsItsCredential. Changing the API base URL should
// not require pasting a private key again.
func TestEditingAConnectionKeepsItsCredential(t *testing.T) {
	key, _ := secrets.NewKey()
	f := newConsole(t, func(o *ConsoleOptions) {
		box, _ := secrets.NewBox(key)
		o.Secrets = box
	})

	if rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/github", f.adminToken, map[string]any{
		"provider": "github", "app_id": "123", "private_key": testPrivateKey,
	}); rec.Code != http.StatusOK {
		t.Fatalf("connecting: %d", rec.Code)
	}
	before, err := f.db.IntegrationSecret(t.Context(), "github")
	if err != nil || before == "" {
		t.Fatalf("nothing stored: %v", err)
	}

	// Same connection, new base URL, no credential supplied.
	if rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/github", f.adminToken, map[string]any{
		"provider": "github", "app_id": "123", "api_base_url": "https://ghe.example",
	}); rec.Code != http.StatusOK {
		t.Fatalf("editing: %d", rec.Code)
	}

	after, err := f.db.IntegrationSecret(t.Context(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("editing a connection without supplying a credential discarded the stored one")
	}
}

// TestConnectingWithoutAnEncryptionKeyIsRefusedWithTheFix. There is nowhere
// safe to put the credential, and no amount of retrying changes that.
func TestConnectingWithoutAnEncryptionKeyIsRefusedWithTheFix(t *testing.T) {
	f := newConsole(t) // no Secrets option: a keyless deployment

	rec, body := f.send(t, http.MethodPut, "/api/v1/integrations/github", f.adminToken, map[string]any{
		"provider": "github", "token": "ghp_x",
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "PANORAMA_ENCRYPTION_KEY") {
		t.Errorf("error = %q, want it to name the setting that fixes this", message)
	}

	// And nothing was written.
	if _, err := f.db.IntegrationSecret(t.Context(), "github"); err == nil {
		t.Error("a row was written for a connection that could not store its credential")
	}
}

// TestOnlyAdministratorsConnect. Connecting decides what this deployment
// reaches out to and with whose access.
func TestOnlyAdministratorsConnect(t *testing.T) {
	key, _ := secrets.NewKey()
	f := newConsole(t, func(o *ConsoleOptions) {
		box, _ := secrets.NewBox(key)
		o.Secrets = box
	})
	_, managerToken := f.newUser(t, "manager@example.com", store.RoleManager)

	for _, tc := range []struct{ name, token string }{
		{"manager", managerToken},
		{"developer", f.devToken},
	} {
		rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/github", tc.token, map[string]any{
			"provider": "github", "token": "ghp_x",
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("a %s connected an integration: %d", tc.name, rec.Code)
		}
		rec, _ = f.send(t, http.MethodDelete, "/api/v1/integrations/github", tc.token, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a %s disconnected an integration: %d", tc.name, rec.Code)
		}
	}
}

// TestDisconnectingRemovesTheCredential. A disabled row holding an encrypted
// credential for a connection nobody intends to use is the thing this design
// exists to avoid.
func TestDisconnectingRemovesTheCredential(t *testing.T) {
	key, _ := secrets.NewKey()
	f := newConsole(t, func(o *ConsoleOptions) {
		box, _ := secrets.NewBox(key)
		o.Secrets = box
	})

	if rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/github", f.adminToken, map[string]any{
		"provider": "github", "token": "ghp_x",
	}); rec.Code != http.StatusOK {
		t.Fatalf("connecting: %d", rec.Code)
	}
	if rec, _ := f.send(t, http.MethodDelete, "/api/v1/integrations/github", f.adminToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("disconnecting: %d", rec.Code)
	}

	if _, err := f.db.IntegrationSecret(t.Context(), "github"); err == nil {
		t.Error("the credential survived disconnection")
	}
	connected, err := f.db.AnyIntegrationConfigured(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if connected {
		t.Error("the deployment still reports an integration connected")
	}
}

// TestAmbiguousCredentialsAreRefused. One credential shape, enforced where the
// form is.
func TestAmbiguousCredentialsAreRefused(t *testing.T) {
	key, _ := secrets.NewKey()
	f := newConsole(t, func(o *ConsoleOptions) {
		box, _ := secrets.NewBox(key)
		o.Secrets = box
	})

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"both shapes", map[string]any{"provider": "github", "app_id": "1", "private_key": testPrivateKey, "token": "ghp_x"}},
		{"key without an app id", map[string]any{"provider": "github", "private_key": testPrivateKey}},
		{"no provider", map[string]any{"token": "ghp_x"}},
		{"unknown provider", map[string]any{"provider": "gitlab", "token": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/github", f.adminToken, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}

	// And GitHub cannot stand in for the ticket half.
	rec, _ := f.send(t, http.MethodPut, "/api/v1/integrations/jira", f.adminToken, map[string]any{
		"provider": "github", "token": "ghp_x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("github accepted for jira: %d", rec.Code)
	}
}
