// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// credential produces the Authorization header value for one request.
//
// Two shapes satisfy it, and they are not equivalent. A personal access token
// is a person's credential with that person's access, which makes it right for
// an evaluation and wrong for a deployment: it carries whatever repositories
// its owner can see, and it stops working when they leave. A GitHub App is
// installed on an organisation with a stated scope, and is what a real
// deployment should use - so it is the documented path, and a PAT is labelled
// as what it is wherever it appears.
type credential interface {
	// header returns the value for Authorization, refreshing if it must.
	header(ctx context.Context, client *http.Client, api string) (string, error)
	// describe names the credential for the integrations screen, without
	// disclosing it.
	describe() string
}

// patCredential is a personal access token or a fine-grained token.
type patCredential struct{ token string }

func (c *patCredential) header(context.Context, *http.Client, string) (string, error) {
	return "Bearer " + c.token, nil
}

func (c *patCredential) describe() string { return "a personal access token" }

// appCredential is a GitHub App installation.
//
// Two credentials in a trench coat: a short-lived JWT signed with the app's
// private key proves which app is calling, and is exchanged for an installation
// token that proves which organisation's data it may read. Only the second can
// read a repository, and it expires in an hour - so it is cached and renewed,
// rather than minted per request.
type appCredential struct {
	appID          string
	installationID string
	key            *rsa.PrivateKey

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (c *appCredential) describe() string {
	return "GitHub App " + c.appID
}

// tokenSkew is how long before expiry an installation token is replaced.
//
// A token that expires between the check and the call is a request that fails
// for a reason nobody can reproduce, so the last minute of its life is not used.
const tokenSkew = time.Minute

func (c *appCredential) header(ctx context.Context, client *http.Client, api string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Add(tokenSkew).Before(c.expires) {
		return "Bearer " + c.token, nil
	}

	assertion, err := c.assertion()
	if err != nil {
		return "", err
	}

	installation := c.installationID
	if installation == "" {
		// Discovered rather than configured. An installation id is not a
		// secret and not a decision - it is a fact about where the app was
		// installed - so asking an operator to find it in a URL is asking them
		// to copy a number that this can look up.
		if installation, err = discoverInstallation(ctx, client, api, assertion); err != nil {
			return "", err
		}
		c.installationID = installation
	}

	token, expires, err := installationToken(ctx, client, api, assertion, installation)
	if err != nil {
		return "", err
	}
	c.token, c.expires = token, expires
	return "Bearer " + token, nil
}

// assertion mints the app JWT.
//
// Hand-rolled rather than pulled from a library: it is one fixed algorithm with
// three claims, and a dependency for that would be more surface than the code
// it replaced. GitHub requires RS256 and rejects anything longer than ten
// minutes; nine is used so a slow clock does not produce a token GitHub reads
// as issued in the future.
func (c *appCredential) assertion() (string, error) {
	now := time.Now()

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		// Backdated by a minute, because GitHub refuses a token whose iat is in
		// its own future and a few seconds of clock drift is ordinary.
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": c.appID,
	})
	if err != nil {
		return "", err
	}

	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)

	digest := crypto.SHA256.New()
	digest.Write([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		return "", fmt.Errorf("github: signing the app assertion: %w", err)
	}

	return signing + "." + enc.EncodeToString(signature), nil
}

func discoverInstallation(ctx context.Context, client *http.Client, api, assertion string) (string, error) {
	var installations []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := appCall(ctx, client, api+"/app/installations", assertion, &installations); err != nil {
		return "", err
	}

	switch len(installations) {
	case 0:
		return "", errors.New("github: this app is not installed anywhere; install it on your organisation first")
	case 1:
		return fmt.Sprintf("%d", installations[0].ID), nil
	default:
		// Refused rather than guessed. Picking the first would silently confirm
		// work items against one organisation while the operator believed it
		// was reading another.
		var where []string
		for _, i := range installations {
			where = append(where, fmt.Sprintf("%s (%d)", i.Account.Login, i.ID))
		}
		return "", fmt.Errorf(
			"github: this app is installed in %d places, so integrations.github.installation_id is required: %s",
			len(installations), strings.Join(where, ", "))
	}
}

func installationToken(ctx context.Context, client *http.Client, api, assertion, installation string) (string, time.Time, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	url := api + "/app/installations/" + installation + "/access_tokens"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	setHeaders(req, "Bearer "+assertion)

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github: minting an installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, statusError(resp, "minting an installation token")
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, fmt.Errorf("github: reading an installation token: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("github: the installation token response carried no token")
	}
	return out.Token, out.ExpiresAt, nil
}

func appCall(ctx context.Context, client *http.Client, url, assertion string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	setHeaders(req, "Bearer "+assertion)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return statusError(resp, "reading app installations")
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// parsePrivateKey reads the PEM GitHub hands out when an app key is generated.
//
// GitHub issues PKCS#1 ("BEGIN RSA PRIVATE KEY"); some tooling converts it to
// PKCS#8 on the way through a secret store. Both are accepted, because an
// operator who pasted a working key into Vault and got a differently-worded one
// back has not made a mistake worth an error message.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github: the private key is not PEM; expected a file beginning -----BEGIN")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: reading the private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github: the private key is %T; GitHub Apps sign with RSA", parsed)
	}
	return key, nil
}
