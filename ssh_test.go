package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestValidateBanditUsername(t *testing.T) {
	for _, username := range []string{"bandit0", "bandit1", "bandit33"} {
		if err := ValidateBanditUsername(username); err != nil {
			t.Errorf("ValidateBanditUsername(%q): %v", username, err)
		}
	}
	for _, username := range []string{"bandit34", "bandit-1", "bandit01", "bandit1;whoami", "root", ""} {
		if err := ValidateBanditUsername(username); err == nil {
			t.Errorf("ValidateBanditUsername(%q) unexpectedly succeeded", username)
		}
	}
}

func TestHostKeyMustBeConfirmedAndIsPersisted(t *testing.T) {
	key := testHostKey(t)
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	prompted := 0
	callback, err := NewBanditHostKeyCallback(knownHosts, func(prompt HostKeyPrompt) bool {
		prompted++
		if prompt.Address != net.JoinHostPort(BanditHost, "2220") {
			t.Errorf("prompt address = %q", prompt.Address)
		}
		if prompt.Fingerprint != ssh.FingerprintSHA256(key) {
			t.Errorf("fingerprint = %q", prompt.Fingerprint)
		}
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2220}
	err = callback(net.JoinHostPort(BanditHost, "2220"), remote, key)
	var promptErr *HostKeyPromptError
	if !errors.As(err, &promptErr) {
		t.Fatalf("expected HostKeyPromptError, got %v", err)
	}
	if prompted != 1 {
		t.Fatalf("confirmation called %d times, want 1", prompted)
	}

	approved, err := NewBanditHostKeyCallback(knownHosts, func(HostKeyPrompt) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if err := approved(net.JoinHostPort(BanditHost, "2220"), remote, key); err != nil {
		t.Fatalf("approved first-use key: %v", err)
	}

	reopened, err := NewBanditHostKeyCallback(knownHosts, func(HostKeyPrompt) bool {
		t.Fatal("confirmation should not be requested for a saved key")
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened(net.JoinHostPort(BanditHost, "2220"), remote, key); err != nil {
		t.Fatalf("saved key was not trusted: %v", err)
	}
}

func TestHostKeyChangeIsRejectedWithoutPrompt(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	first, changed := testHostKey(t), testHostKey(t)
	callback, err := NewBanditHostKeyCallback(knownHosts, func(HostKeyPrompt) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort(BanditHost, "2220")
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2220}
	if err := callback(addr, remote, first); err != nil {
		t.Fatal(err)
	}
	prompted := false
	reopened, err := NewBanditHostKeyCallback(knownHosts, func(HostKeyPrompt) bool {
		prompted = true
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened(addr, remote, changed); err == nil {
		t.Fatal("changed host key was accepted")
	}
	if prompted {
		t.Fatal("changed key must be rejected, not offered as a new key")
	}
}

func TestHostKeyCallbackRejectsOtherEndpoint(t *testing.T) {
	key := testHostKey(t)
	prompted := false
	callback, err := NewBanditHostKeyCallback(filepath.Join(t.TempDir(), "known_hosts"), func(HostKeyPrompt) bool {
		prompted = true
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := callback("attacker.example:22", remote, key); err == nil {
		t.Fatal("unexpected SSH endpoint was accepted")
	}
	if prompted {
		t.Fatal("unexpected endpoint must not trigger a trust prompt")
	}
}

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	return key
}
