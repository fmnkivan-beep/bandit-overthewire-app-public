package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type keyTestSSHInput struct{ strings.Builder }

func (input *keyTestSSHInput) Close() error { return nil }

type keyTestSSHEchoInput struct{ echo func(string) }

func (input *keyTestSSHEchoInput) Write(data []byte) (int, error) {
	input.echo(string(data))
	return len(data), nil
}

func (input *keyTestSSHEchoInput) Close() error { return nil }

func TestSSHInputIsGuardedBeforeTransportEcho(t *testing.T) {
	const marker = liveKeyFixture
	app := NewApp(newEventHub(), "")
	matcher := &keyOutputMatcher{level: 1, expected: marker}
	session := &BanditSession{}
	session.Input = &keyTestSSHEchoInput{echo: func(data string) { app.emitSSHKeyOutput(session, matcher, data) }}
	app.sshSession = session
	if err := app.SendSSHData(marker + "\r\n"); err != nil {
		t.Fatal(err)
	}
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("transport echo raced ahead of the input guard")
	}
}

func TestSSHInputEchoDoesNotUnlockKeys(t *testing.T) {
	const marker = liveKeyFixture
	for _, input := range []string{marker + "\r", "printf '" + marker + "'\r"} {
		for _, split := range []bool{false, true} {
			app := NewApp(newEventHub(), "")
			app.sshSession = &BanditSession{Input: &keyTestSSHInput{}}
			matcher := &keyOutputMatcher{level: 1, expected: marker}
			events, stop := app.events.subscribe()
			if split {
				for _, c := range input {
					if err := app.SendSSHData(string(c)); err != nil {
						t.Fatal(err)
					}
					app.emitSSHKeyOutput(app.sshSession, matcher, string(c))
				}
				app.emitSSHKeyOutput(app.sshSession, matcher, "\n")
			} else {
				if err := app.SendSSHData(input); err != nil {
					t.Fatal(err)
				}
				app.emitSSHKeyOutput(app.sshSession, matcher, input+"\n")
			}
			// Session contamination must survive reset and later occurrences of the same token.
			app.resetUnlockedKeys()
			app.emitSSHKeyOutput(app.sshSession, matcher, marker+"\n")
			if len(app.unlockedKeysSnapshot()) != 0 {
				t.Fatal("SSH input echo unlocked a key")
			}
			rendered := ""
			for len(events) > 0 {
				event := <-events
				if event.name == "keys:unlocked" {
					t.Fatal("SSH input echo emitted an unlock event")
				}
				if event.name == "ssh:data" {
					rendered += event.data.(string)
				}
			}
			if !strings.Contains(rendered, marker) {
				t.Fatal("echo guard changed visible terminal output")
			}
			stop()
		}
	}
}

func TestSSHInputGuardAllowsNormalCommandOutput(t *testing.T) {
	const marker = liveKeyFixture
	app := NewApp(newEventHub(), "")
	app.sshSession = &BanditSession{Input: &keyTestSSHInput{}}
	matcher := &keyOutputMatcher{level: 1, expected: marker}
	if err := app.SendSSHData("cat /etc/bandit_pass/bandit1\r"); err != nil {
		t.Fatal(err)
	}
	app.emitSSHKeyOutput(app.sshSession, matcher, "cat /etc/bandit_pass/bandit1\r\n")
	app.emitSSHKeyOutput(app.sshSession, matcher, marker[:8])
	app.emitSSHKeyOutput(app.sshSession, matcher, marker[8:]+"\r\n")
	if len(app.unlockedKeysSnapshot()) != 1 {
		t.Fatal("normal command output did not unlock its key")
	}
}

func TestSSHInputGuardIsBoundedAndHandlesConcurrentOutput(t *testing.T) {
	const marker = liveKeyFixture
	app := NewApp(newEventHub(), "")
	app.sshSession = &BanditSession{Input: &keyTestSSHInput{}}
	matcher := &keyOutputMatcher{level: 1, expected: marker}
	if err := app.SendSSHData(strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	if len(app.sshKeyInput.tail) > 31 {
		t.Fatal("input guard retained an unbounded tail")
	}
	if err := app.SendSSHData(marker[:8]); err != nil {
		t.Fatal(err)
	}
	if err := app.SendSSHData(marker[8:]); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 50; i++ {
			_ = app.SendSSHData("\r")
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 50; i++ {
			app.emitSSHKeyOutput(app.sshSession, matcher, marker+"\n")
		}
	}()
	workers.Wait()
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("concurrent echo unlocked a key")
	}
}

func TestSSHInputGuardOverflowFailsClosed(t *testing.T) {
	app := NewApp(newEventHub(), "")
	session := &BanditSession{Input: &keyTestSSHInput{}}
	app.sshSession = session
	tokens := make([]string, 65)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("typedcandidate%018d", i)
	}
	if err := app.SendSSHData(strings.Join(tokens, " ")); err != nil {
		t.Fatal(err)
	}
	if !app.sshKeyInput.overflow || app.sshKeyInput.typedCount != len(app.sshKeyInput.typed) {
		t.Fatal("input candidate overflow was not detected")
	}
	app.emitSSHKeyOutput(session, app.newKeyMatcher(1), banditPasswords[1]+"\n")
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("overflowed input guard allowed an echoed key to unlock")
	}
}

func TestSSHInputGuardResetsOnlyForNewSession(t *testing.T) {
	app := NewApp(nil, "")
	app.sshKeyInput.observe(liveKeyFixture)
	app.resetUnlockedKeys()
	if !app.sshKeyInput.blocks(liveKeyFixture) {
		t.Fatal("key reset cleared input echo protection")
	}
	if err := app.activateSSHSession(context.Background(), &BanditSession{}, "bandit0", banditPasswords[0]); err != nil {
		t.Fatal(err)
	}
	if app.sshKeyInput.blocks(liveKeyFixture) || app.sshKeyInput.tail != "" {
		t.Fatal("new SSH session did not initialize its input guard")
	}
	if err := app.DisconnectSSH(); err != nil {
		t.Fatal(err)
	}
	if app.sshKeyInput.blocks(liveKeyFixture) || app.sshKeyInput.tail != "" {
		t.Fatal("disconnect retained the input guard")
	}
	app.sshKeyInput.observe("Echo")
	if err := app.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if app.sshKeyInput.tail != "" {
		t.Fatal("shutdown retained the input guard")
	}
}

func TestKnownBanditPasswords(t *testing.T) {
	seen := map[string]bool{}
	for level := 0; level < 34; level++ {
		password, ok := knownBanditPassword(level)
		if !ok || password == "" || seen[password] {
			t.Fatalf("invalid credential at level %d", level)
		}
		seen[password] = true
	}
	for _, level := range []int{-1, 34, 99} {
		if password, ok := knownBanditPassword(level); ok || password != "" {
			t.Fatalf("accepted index %d", level)
		}
	}
}

func TestKeyOutputMatcher(t *testing.T) {
	password, _ := knownBanditPassword(1)
	wrong, _ := knownBanditPassword(2)
	cases := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{"split", []string{"prefix: " + password[:7], password[7:], "\r\n"}, true},
		{"wrong-level", []string{wrong + "\n"}, false},
		{"partial", []string{password[:len(password)-1] + "\n"}, false},
		{"prefix", []string{"x" + password + "\n"}, false},
		{"suffix", []string{password + "x\n"}, false},
		{"underscore", []string{"_" + password + "_\n"}, false},
		{"chunk-end", []string{password}, true},
		{"chunk-end-then-token-continuation", []string{password, "x\n"}, true},
		{"long-token", []string{strings.Repeat("a", 100000) + password + "\n"}, false},
		{"unicode-token", []string{strings.Repeat("я", 1000)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matcher := newKeyOutputMatcher(1, 0)
			found := false
			for _, chunk := range tc.chunks {
				found = matcher.consume(chunk) || found
				if len(matcher.tail) > len(password)+1 {
					t.Fatal("unbounded matcher tail")
				}
			}
			if found != tc.want {
				t.Fatalf("match = %t, want %t", found, tc.want)
			}
		})
	}
}

func TestUnlockedKeyLifecycle(t *testing.T) {
	app := NewApp(newEventHub(), "")
	events, stop := app.events.subscribe()
	defer stop()
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("new app has keys")
	}
	app.unlockKey(2)
	app.unlockKey(2)
	app.unlockKey(-1)
	app.unlockKey(34)
	keys := app.unlockedKeysSnapshot()
	if len(keys) != 1 || keys[0].Level != 2 || keys[0].Username != "bandit2" {
		t.Fatal("incorrect unlocked snapshot")
	}
	keys[0].Password = "changed"
	if app.unlockedKeysSnapshot()[0].Password == "changed" {
		t.Fatal("snapshot aliases stored keys")
	}
	if len(events) != 1 || (<-events).name != "keys:unlocked" {
		t.Fatal("unlock not idempotent")
	}
	app.gitPassword = "test-only"
	app.resetUnlockedKeys()
	if len(app.unlockedKeysSnapshot()) != 0 || app.gitPassword != "test-only" {
		t.Fatal("reset touched unrelated state")
	}
	app.unlockKey(1)
	if err := app.Shutdown(); err != nil {
		t.Fatal(err)
	}
	app.unlockKey(2)
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("shutdown retained or reopened keys")
	}
}

func TestSSHKeyAuthenticationAndSessionBinding(t *testing.T) {
	app := NewApp(newEventHub(), "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rejected := &BanditSession{}
	if err := app.activateSSHSession(ctx, rejected, "bandit3", banditPasswords[3]); err == nil {
		t.Fatal("cancelled authentication activated")
	}
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("failed login unlocked key")
	}
	session := &BanditSession{}
	if err := app.activateSSHSession(context.Background(), session, "bandit3", banditPasswords[3]); err != nil {
		t.Fatal(err)
	}
	keys := app.unlockedKeysSnapshot()
	if len(keys) != 1 || keys[0].Level != 3 {
		t.Fatal("login unlocked wrong credential")
	}
	events, stop := app.events.subscribe()
	defer stop()
	next, _ := knownBanditPassword(4)
	wrong, _ := knownBanditPassword(5)
	matcher := app.newKeyMatcher(4)
	app.emitSSHKeyOutput(rejected, matcher, next+"\n")
	app.emitSSHKeyOutput(session, matcher, wrong+"\n")
	app.emitSSHKeyOutput(session, matcher, next[:9])
	app.emitSSHKeyOutput(session, matcher, next[9:])
	if keys = app.unlockedKeysSnapshot(); len(keys) != 2 || keys[1].Level != 4 {
		t.Fatal("session output unlocked wrong keys")
	}
	last := ""
	for len(events) > 0 {
		event := <-events
		if event.name == "keys:unlocked" && last != "ssh:data" {
			t.Fatal("key preceded terminal output")
		}
		last = event.name
	}
	app.sshSession = nil // Avoid closing the deliberately transport-free test session.
}

func TestSSHOutputUnlocksNextKeyWithoutFollowingLogin(t *testing.T) {
	app := NewApp(newEventHub(), "")
	session := &BanditSession{}
	if err := app.activateSSHSession(context.Background(), session, "bandit0", banditPasswords[0]); err != nil {
		t.Fatal(err)
	}
	password, _ := knownBanditPassword(1)
	matcher := app.newKeyMatcher(1)
	app.emitSSHKeyOutput(session, matcher, "readme: "+password[:12])
	if len(app.unlockedKeysSnapshot()) != 1 {
		t.Fatal("partial terminal output unlocked the target key")
	}
	app.emitSSHKeyOutput(session, matcher, password[12:])
	keys := app.unlockedKeysSnapshot()
	if len(keys) != 2 || keys[1].Level != 1 || keys[1].Password != password {
		t.Fatal("complete terminal output did not unlock the target key before another login")
	}
	app.sshSession = nil // Avoid closing the deliberately transport-free test session.
}

func TestSSHStreamReaderPathUnlocksKeyBeforeNewLogin(t *testing.T) {
	app := NewApp(newEventHub(), "")
	password, _ := knownBanditPassword(1)
	session := &BanditSession{
		Input:  &keyTestSSHInput{},
		Output: io.MultiReader(strings.NewReader("cat readme\r\n"), strings.NewReader(password[:12]), strings.NewReader(password[12:]+"\r\n")),
		Errors: strings.NewReader(""),
	}
	events, stop := app.events.subscribe()
	defer stop()
	if err := app.activateSSHSession(context.Background(), session, "bandit0", banditPasswords[0]); err != nil {
		t.Fatal(err)
	}
	if err := app.SendSSHData("cat readme\r"); err != nil {
		t.Fatal(err)
	}
	app.streamSSH(session) // Exercise the production reader goroutines; no ConnectSSH or next login is used.

	keys := app.unlockedKeysSnapshot()
	if len(keys) != 2 || keys[1].Level != 1 || keys[1].Password != password {
		t.Fatal("streamSSH did not unlock key1 from cat readme output")
	}
	terminalOutput := ""
	key1Event := false
	for len(events) > 0 {
		event := <-events
		switch event.name {
		case "ssh:data":
			terminalOutput += event.data.(string)
		case "keys:unlocked":
			if event.data.(unlockedKey).Level == 1 {
				key1Event = true
			}
		}
	}
	if !strings.Contains(terminalOutput, "cat readme\r\n"+password+"\r\n") || !key1Event {
		t.Fatal("terminal output or the key1 unlock event did not pass through streamSSH")
	}
}

func TestKeyMatchersResetAndGitIsolation(t *testing.T) {
	app := NewApp(newEventHub(), "")
	session := &BanditSession{}
	app.sshSession = session
	password, _ := knownBanditPassword(1)
	matcher := app.newKeyMatcher(1)
	app.emitSSHKeyOutput(session, matcher, password[:8])
	app.resetUnlockedKeys()
	app.emitSSHKeyOutput(session, matcher, password[8:]+"\n")
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("partial key survived reset")
	}
	for level := 27; level <= 33; level++ {
		app.resetUnlockedKeys()
		key, _ := knownBanditPassword(level)
		other, _ := knownBanditPassword(1)
		gitMatcher := app.newKeyMatcher(level)
		app.emitGitKeyOutput(gitMatcher, app.gitOutputGeneration, other+"\n")
		app.emitGitKeyOutput(gitMatcher, app.gitOutputGeneration, key+"\n")
		expected := 0
		if level >= 28 && level <= 32 {
			expected = 1
		}
		if len(app.unlockedKeysSnapshot()) != expected {
			t.Fatalf("Git target isolation failed for %d", level)
		}
	}
	app.resetUnlockedKeys()
	gitMatcher := app.newKeyMatcher(28)
	generation := app.gitOutputGeneration
	app.DisconnectGit()
	key, _ := knownBanditPassword(28)
	app.emitGitKeyOutput(gitMatcher, generation, key+"\n")
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("disconnected Git output unlocked key")
	}
	app.sshSession = nil
}

func TestSSHLoginLevels(t *testing.T) {
	for level := 0; level < 34; level++ {
		app := NewApp(nil, "")
		if err := app.activateSSHSession(context.Background(), &BanditSession{}, fmt.Sprintf("bandit%d", level), banditPasswords[level]); err != nil {
			t.Fatal(err)
		}
		keys := app.unlockedKeysSnapshot()
		if len(keys) != 1 || keys[0].Level != level {
			t.Fatalf("login target %d mismatch", level)
		}
	}
}

func TestGitFileOutputUnlocksTargetAtEOF(t *testing.T) {
	app := NewApp(newEventHub(), "")
	app.workspace = t.TempDir()
	repo := filepath.Join(app.workspace, "level-28")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	password, _ := knownBanditPassword(28)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(password), 0600); err != nil {
		t.Fatal(err)
	}
	events, stop := app.events.subscribe()
	defer stop()
	if err := app.RunGitCommand(28, "cat README.md"); err != nil {
		t.Fatal(err)
	}
	keys := app.unlockedKeysSnapshot()
	if len(keys) != 1 || keys[0].Level != 28 {
		t.Fatal("Git cat did not unlock its target")
	}
	if len(events) != 2 || (<-events).name != "git:output" || (<-events).name != "keys:unlocked" {
		t.Fatal("Git output/event order invalid")
	}
}

func TestSSHStreamsDoNotCombineTokens(t *testing.T) {
	app := NewApp(newEventHub(), "")
	session := &BanditSession{}
	app.sshSession = session
	password, _ := knownBanditPassword(1)
	stdout, stderr := app.newKeyMatcher(1), app.newKeyMatcher(1)
	app.emitSSHKeyOutput(session, stdout, password[:8])
	app.emitSSHKeyOutput(session, stderr, password[8:]+"\n")
	if len(app.unlockedKeysSnapshot()) != 0 {
		t.Fatal("stdout and stderr combined a key")
	}
	app.emitSSHKeyOutput(session, stdout, password[8:])
	if len(app.unlockedKeysSnapshot()) != 1 {
		t.Fatal("complete key at output chunk end did not unlock")
	}
	app.sshSession = nil
}

const liveKeyFixture = "FixturePassword0123456789ABCDEFG"

func waitKeyCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for key verification")
}

func TestActualOutputPasswordVerification(t *testing.T) {
	for _, source := range []string{"ssh", "git"} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", source, success), func(t *testing.T) {
				app := NewApp(newEventHub(), "")
				defer app.Shutdown()
				calls := make(chan bool, 1)
				app.verifyBanditPassword = func(ctx context.Context, user, password, path string) error {
					if password != liveKeyFixture || user != "bandit1" && user != "bandit28" {
						return errors.New("unexpected candidate")
					}
					calls <- true
					if !success {
						return errors.New("authentication rejected")
					}
					return nil
				}
				session := &BanditSession{}
				app.sshSession = session
				if source == "ssh" {
					app.emitSSHKeyOutput(session, app.newKeyMatcher(1), "password: "+liveKeyFixture)
				} else {
					app.emitGitKeyOutput(app.newKeyMatcher(28), 0, liveKeyFixture+"\n")
				}
				select {
				case <-calls:
				case <-time.After(time.Second):
					t.Fatal("no verifier call")
				}
				waitKeyCondition(t, func() bool { app.mu.Lock(); defer app.mu.Unlock(); return !app.keyVerification.running })
				keys := app.unlockedKeysSnapshot()
				if success {
					if len(keys) != 1 || keys[0].Password != liveKeyFixture {
						t.Fatal("actual verified key not archived")
					}
				} else if len(keys) != 0 {
					t.Fatal("failed candidate unlocked")
				}
			})
		}
	}
}

func TestCandidateControlsEchoAndNoisyBatch(t *testing.T) {
	app := NewApp(nil, "")
	defer app.Shutdown()
	calls := make(chan bool, 8)
	app.verifyBanditPassword = func(context.Context, string, string, string) error { calls <- true; return nil }
	session := &BanditSession{Input: &keyTestSSHInput{}}
	app.sshSession = session
	matcher := app.newKeyMatcher(1)
	app.emitSSHKeyOutput(session, matcher, "\x1b]3008;"+liveKeyFixture[:10])
	app.emitSSHKeyOutput(session, matcher, liveKeyFixture[10:]+"\x1b\\\n\x1b[32mnormal\x1b[0m\n")
	time.Sleep(300 * time.Millisecond)
	if len(calls) != 0 {
		t.Fatal("OSC metadata reached verifier")
	}
	app.emitSSHKeyOutput(session, matcher, strings.Repeat(liveKeyFixture+" "+strings.Repeat("x", 32)+"\n", 1000))
	time.Sleep(350 * time.Millisecond)
	if len(calls) != 0 {
		t.Fatal("noisy batch produced auth attempts")
	}
	if err := app.SendSSHData("echo " + banditPasswords[1] + "\r"); err != nil {
		t.Fatal(err)
	}
	app.emitSSHKeyOutput(session, matcher, liveKeyFixture+"\n")
	waitKeyCondition(t, func() bool { return len(app.unlockedKeysSnapshot()) == 1 })
	app.resetUnlockedKeys()
	app.emitSSHKeyOutput(session, matcher, liveKeyFixture+"\n")
	waitKeyCondition(t, func() bool { return len(app.unlockedKeysSnapshot()) == 1 })
	app.resetUnlockedKeys()
	if err := app.SendSSHData("echo " + liveKeyFixture + "\r"); err != nil {
		t.Fatal(err)
	}
	app.emitSSHKeyOutput(session, app.newKeyMatcher(1), liveKeyFixture+"\n")
	time.Sleep(300 * time.Millisecond)
	if len(app.unlockedKeysSnapshot()) != 0 || len(calls) != 2 {
		t.Fatal("literal input echo opened key")
	}
}

func TestResetClearsVerificationCooldownAndChecksChunkEndCandidate(t *testing.T) {
	app := NewApp(nil, "")
	defer app.Shutdown()
	verified := make(chan bool, 1)
	app.verifyBanditPassword = func(context.Context, string, string, string) error {
		verified <- true
		return nil
	}
	app.keyVerification.nextAttempt = time.Now().Add(time.Hour)
	app.resetUnlockedKeys()
	app.sshSession = &BanditSession{}
	app.emitSSHKeyOutput(app.sshSession, app.newKeyMatcher(1), liveKeyFixture)
	select {
	case <-verified:
	case <-time.After(time.Second):
		t.Fatal("reset retained the old cooldown or chunk-end candidate was missed")
	}
	waitKeyCondition(t, func() bool { return len(app.unlockedKeysSnapshot()) == 1 })
}

func TestVerificationInvalidatedByLifecycle(t *testing.T) {
	for _, action := range []string{"reset", "disconnect", "shutdown", "git-disconnect"} {
		t.Run(action, func(t *testing.T) {
			app := NewApp(nil, "")
			defer app.Shutdown()
			started, release, done := make(chan bool, 1), make(chan bool), make(chan bool, 1)
			app.verifyBanditPassword = func(ctx context.Context, u, p, k string) error {
				started <- true
				<-release
				done <- ctx.Err() != nil
				return nil
			}
			session := &BanditSession{}
			app.sshSession = session
			if action == "git-disconnect" {
				app.emitGitKeyOutput(app.newKeyMatcher(28), 0, liveKeyFixture+"\n")
			} else {
				app.emitSSHKeyOutput(session, app.newKeyMatcher(1), liveKeyFixture+"\n")
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("worker did not start")
			}
			switch action {
			case "reset":
				app.resetUnlockedKeys()
			case "disconnect":
				app.DisconnectSSH()
			case "shutdown":
				app.Shutdown()
			case "git-disconnect":
				app.DisconnectGit()
			}
			close(release)
			if !<-done {
				t.Fatal("in-flight verifier not canceled")
			}
			waitKeyCondition(t, func() bool { app.mu.Lock(); defer app.mu.Unlock(); return !app.keyVerification.running })
			if len(app.unlockedKeysSnapshot()) != 0 {
				t.Fatal("stale verification reopened key")
			}
		})
	}
}

func TestManualAuthenticationDoesNotStoreUnknownOrOverwriteVerifiedPassword(t *testing.T) {
	app := NewApp(newEventHub(), "")
	defer app.Shutdown()
	if err := app.activateSSHSession(context.Background(), &BanditSession{}, "bandit1", liveKeyFixture); err != nil {
		t.Fatal(err)
	}
	if keys := app.unlockedKeysSnapshot(); len(keys) != 0 {
		t.Fatal("manual authentication archived an unknown password")
	}
	if err := app.DisconnectSSH(); err != nil {
		t.Fatal(err)
	}
	app.unlockVerifiedKeyLocked(1, liveKeyFixture)
	events, stop := app.events.subscribe()
	defer stop()
	if err := app.activateSSHSession(context.Background(), &BanditSession{}, "bandit1", banditPasswords[1]); err != nil {
		t.Fatal(err)
	}
	session := app.sshSession
	app.emitSSHKeyOutput(session, app.newKeyMatcher(1), banditPasswords[1]+"\n")
	keys := app.unlockedKeysSnapshot()
	if len(keys) != 1 || keys[0].Password != liveKeyFixture {
		t.Fatal("manual login or known-password fastpath replaced a verified key")
	}
	for len(events) > 0 {
		event := <-events
		if event.name == "keys:unlocked" {
			t.Fatal("stale password emitted a replacement key event")
		}
	}
}
