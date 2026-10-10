// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestSealAndOpenRoundTrip(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewBox(key)
	if err != nil {
		t.Fatal(err)
	}

	const secret = "-----BEGIN RSA PRIVATE KEY-----\nnot really\n-----END RSA PRIVATE KEY-----"
	sealed, err := box.Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "PRIVATE KEY") {
		t.Fatal("the sealed value contains the plaintext")
	}

	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != secret {
		t.Errorf("round trip changed the value")
	}
}

// TestSealIsNonDeterministic. The same credential stored twice must not produce
// the same row, or the database reveals that two deployments share a token.
func TestSealIsNonDeterministic(t *testing.T) {
	key, _ := NewKey()
	box, _ := NewBox(key)

	first, err := box.Seal("ghp_the_same_token")
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.Seal("ghp_the_same_token")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("sealing the same value twice produced identical ciphertext")
	}
}

// TestAnotherKeyCannotRead is the property that makes storing a credential in a
// database defensible: the dump is worthless without the key, which was never
// in it.
func TestAnotherKeyCannotRead(t *testing.T) {
	mine, _ := NewKey()
	theirs, _ := NewKey()

	box, _ := NewBox(mine)
	other, _ := NewBox(theirs)

	sealed, err := box.Seal("ghp_secret")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := other.Open(sealed); !errors.Is(err, ErrWrongKey) {
		t.Errorf("err = %v, want ErrWrongKey - a different key must not read it", err)
	}
}

// TestTamperingIsDetected. GCM authenticates, so a modified ciphertext is
// refused rather than decrypted into something else.
func TestTamperingIsDetected(t *testing.T) {
	key, _ := NewKey()
	box, _ := NewBox(key)

	sealed, _ := box.Seal("ghp_secret")
	tampered := []byte(sealed)
	tampered[len(tampered)-2] ^= 'A'

	if _, err := box.Open(string(tampered)); err == nil {
		t.Error("a tampered value was accepted")
	}
}

// TestNoKeyIsUsableButRefusesToStore. A deployment that has not configured a
// key still runs; it just cannot hold a credential, and says so.
func TestNoKeyIsUsableButRefusesToStore(t *testing.T) {
	box, err := NewBox("")
	if err != nil {
		t.Fatalf("an empty key must not be an error: %v", err)
	}
	if box.Configured() {
		t.Error("a keyless box reports itself configured")
	}
	if _, err := box.Seal("x"); !errors.Is(err, ErrNoKey) {
		t.Errorf("Seal = %v, want ErrNoKey", err)
	}
	if _, err := box.Open("x"); !errors.Is(err, ErrNoKey) {
		t.Errorf("Open = %v, want ErrNoKey", err)
	}
}

// TestAShortKeyIsRefusedAndSaysWhy. Anything is accepted as a key, so the only
// thing left to refuse is one too short for the derivation to be worth
// anything - and the refusal has to name both ways out.
func TestAShortKeyIsRefusedAndSaysWhy(t *testing.T) {
	short, err := NewBox("c2hvcnQ=") // "short", and eight characters either way
	if err == nil {
		t.Fatalf("a short key was accepted: %+v", short)
	}
	if !strings.Contains(err.Error(), "generate-key") {
		t.Errorf("err = %v, want it to name the command that fixes it", err)
	}
	if !strings.Contains(err.Error(), "phrase") {
		t.Errorf("err = %v, want it to say a passphrase also works", err)
	}
}

// TestAPassphraseIsAKey is the whole point of the change: nobody should have to
// run a command to put a value in a configuration file.
func TestAPassphraseIsAKey(t *testing.T) {
	box, err := NewBox("not base64!! but long enough to be a passphrase")
	if err != nil {
		t.Fatalf("a passphrase was refused: %v", err)
	}
	if !box.Derived() {
		t.Error("a passphrase-backed box does not report itself derived")
	}

	sealed, err := box.Seal("ghp_secret")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != "ghp_secret" {
		t.Errorf("round trip changed the value")
	}
}

// TestDerivationIsStable. The derivation is the only way back to a credential
// already stored, so the same passphrase must give the same key forever - a
// change here is a silent data loss, which is why it is pinned to a literal
// rather than compared against a second call.
func TestDerivationIsStable(t *testing.T) {
	key, derived, err := DeriveKey("a passphrase somebody chose")
	if err != nil {
		t.Fatal(err)
	}
	if !derived {
		t.Fatal("DeriveKey did not report deriving")
	}
	const want = "4fpOl7NBIO18siWceYgZpvuYDi88mErrIwopwh7tXvE="
	if got := base64.StdEncoding.EncodeToString(key); got != want {
		t.Errorf("derived key = %q, want %q - changing the derivation orphans every stored credential", got, want)
	}
}

// TestAMintedKeyIsUsedAsMinted. A key that is already 32 bytes must be those
// 32 bytes, not a derivation of the text of them, or every deployment holding a
// minted key loses its stored credentials.
func TestAMintedKeyIsUsedAsMinted(t *testing.T) {
	minted, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	key, derived, err := DeriveKey(minted)
	if err != nil {
		t.Fatal(err)
	}
	if derived {
		t.Error("a minted key was derived from rather than used")
	}
	raw, err := base64.StdEncoding.DecodeString(minted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, raw) {
		t.Error("a minted key was not used verbatim")
	}
}

// TestTwoPassphrasesDoNotCollide.
func TestTwoPassphrasesDoNotCollide(t *testing.T) {
	box, _ := NewBox("the first passphrase, long enough")
	other, _ := NewBox("the second passphrase, long enough")

	sealed, err := box.Seal("ghp_secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(sealed); !errors.Is(err, ErrWrongKey) {
		t.Errorf("err = %v, want ErrWrongKey", err)
	}
}
