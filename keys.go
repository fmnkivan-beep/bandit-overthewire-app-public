package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type unlockedKey struct {
	Level    int    `json:"level"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func knownBanditPassword(level int) (string, bool) {
	if level < 0 || level >= len(banditPasswords) {
		return "", false
	}
	return banditPasswords[level], true
}

type keyOutputMatcher struct {
	level         int
	expected      string
	tail          string
	epoch         uint64
	matched       bool
	escape        byte
	candidateTail string
	candidateLong bool
}

func (m *keyOutputMatcher) consume(chunk string) bool {
	password := m.expected
	if password == "" || m.matched {
		return false
	}
	found := false
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c >= 128 {
			m.tail += chunk[i : i+1]
			if len(m.tail) > len(password)+1 {
				m.tail = m.tail[1:]
			}
		} else {
			if m.tail == password {
				found = true
				m.matched = true
			}
			m.tail = ""
		}
	}
	if !m.matched && m.tail == password {
		// shortcut: a later read may extend this token; use remote task proof if that false positive becomes unacceptable.
		m.matched = true
		return true
	}
	return found
}

// candidates ignores terminal control payloads, including OSC machine identifiers.
func (m *keyOutputMatcher) candidates(chunk string) (string, []string) {
	var plain strings.Builder
	var found []string
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch m.escape {
		case 1:
			switch c {
			case '[':
				m.escape = 2
			case ']':
				m.escape = 3
			default:
				m.escape = 0
			}
			continue
		case 2:
			if c >= 0x40 && c <= 0x7e {
				m.escape = 0
			}
			continue
		case 3:
			if c == 7 {
				m.escape = 0
			} else if c == 27 {
				m.escape = 4
			}
			continue
		case 4:
			if c == '\\' {
				m.escape = 0
			} else {
				m.escape = 3
			}
			continue
		}
		if c == 27 {
			m.escape = 1
			continue
		}
		plain.WriteByte(c)
		token := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c >= 128
		if token {
			if len(m.candidateTail) < 32 {
				m.candidateTail += string(c)
			} else {
				m.candidateLong = true
			}
		} else {
			if len(m.candidateTail) == 32 && !m.candidateLong && !strings.ContainsAny(m.candidateTail, "_") {
				ascii := true
				for j := range m.candidateTail {
					if m.candidateTail[j] >= 128 {
						ascii = false
					}
				}
				if ascii {
					if len(found) < 2 && (len(found) == 0 || found[0] != m.candidateTail) {
						found = append(found, m.candidateTail)
					}
				}
			}
			m.candidateTail, m.candidateLong = "", false
		}
	}
	if m.escape == 0 && len(m.candidateTail) == 32 && !m.candidateLong && len(found) < 2 && (len(found) == 0 || found[0] != m.candidateTail) {
		valid := true
		for _, c := range m.candidateTail {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				valid = false
			}
		}
		if valid {
			found = append(found, m.candidateTail)
		}
	}
	return plain.String(), found
}

type keyInputGuard struct {
	tail       string
	overflow   bool
	typed      [64][32]byte
	typedCount int
}

func (g *keyInputGuard) observe(data string) {
	if g.overflow {
		return
	}
	input := g.tail + data
	for i := 0; i+32 <= len(input); i++ {
		token := true
		for _, c := range input[i : i+32] {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				token = false
				break
			}
		}
		if token {
			hash := sha256.Sum256([]byte(input[i : i+32]))
			if !g.blocks(input[i : i+32]) {
				if g.typedCount == len(g.typed) {
					g.overflow = true
					g.tail = ""
					return
				}
				g.typed[g.typedCount] = hash
				g.typedCount++
			}
		}
	}
	if len(input) > 31 {
		input = input[len(input)-31:]
	}
	g.tail = input
}

func (g *keyInputGuard) blocks(password string) bool {
	if g.overflow {
		return true
	}
	hash := sha256.Sum256([]byte(password))
	for i := 0; i < g.typedCount; i++ {
		if g.typed[i] == hash {
			return true
		}
	}
	return false
}

func newKeyOutputMatcher(level int, epoch uint64) *keyOutputMatcher {
	expected, _ := knownBanditPassword(level)
	return &keyOutputMatcher{level: level, expected: expected, epoch: epoch}
}

func (a *App) unlockedKeysSnapshot() []unlockedKey {
	a.mu.Lock()
	defer a.mu.Unlock()
	return keysInOrder(a.unlockedKeys)
}

func (a *App) keysSnapshot() ([]unlockedKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.storageReady {
		keys, err := a.readUnlockedKeys()
		if err != nil {
			return nil, err
		}
		return keys, nil
	}
	return keysInOrder(a.unlockedKeys), a.keysStoreErr
}

func (a *App) unlockKey(level int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unlockKeyLocked(level)
}

func (a *App) unlockKeyLocked(level int) {
	if _, exists := a.unlockedKeys[level]; exists {
		return
	}
	password, _ := knownBanditPassword(level)
	a.unlockVerifiedKeyLocked(level, password)
}

func (a *App) unlockVerifiedKeyLocked(level int, password string) {
	if level < 0 || level >= len(banditPasswords) || password == "" || a.keysClosed {
		return
	}
	if key, exists := a.unlockedKeys[level]; exists && key.Password == password {
		return
	}
	if a.unlockedKeys == nil {
		a.unlockedKeys = make(map[int]unlockedKey)
	}
	key := unlockedKey{Level: level, Username: fmt.Sprintf("bandit%d", level), Password: password}
	if !validUnlockedKey(key) {
		return
	}
	a.unlockedKeys[level] = key
	if err := a.persistUnlockedKeyLocked(key); err != nil {
		if a.unsavedKeys == nil {
			a.unsavedKeys = make(map[int]unlockedKey)
		}
		a.unsavedKeys[key.Level] = key
		a.emit("keys:storage-error", map[string]string{"message": "Не удалось сохранить подтверждённый ключ на этом устройстве."})
	}
	a.emit("keys:unlocked", key)
}

func (a *App) resetUnlockedKeys() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.storageReady {
		err := withStorageLock(a.storageDir, func() error {
			progress, err := readProgressFile(a.progressFile)
			if err != nil {
				return err
			}
			if progress.Revision != a.progress.Revision {
				return errStaleProgress
			}
			if _, err := readKeysFileWithRevision(a.keysFile); err != nil {
				return err
			}
			return writeStoredAtomic(a.keysFile, savedKeys{Version: progressVersion, Unlocked: []unlockedKey{}, Revision: progress.Revision}, keysFileMax)
		})
		if err != nil {
			a.keysStoreErr = err
			return err
		}
		a.keysStoreErr = nil
	}
	clear(a.unlockedKeys)
	clear(a.unsavedKeys)
	a.keysEpoch++
	a.cancelKeyVerificationLocked()
	return nil
}

// This is called only after ConnectBandit has authenticated the SSH transport.
func (a *App) activateSSHSession(ctx context.Context, session *BanditSession, username string, authenticatedPassword string) error {
	if err := ValidateBanditUsername(username); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.keysClosed || session == nil || a.sshSession != nil {
		return errors.New("SSH-подключение уже запущено или закрыто")
	}
	a.sshSession, a.sshUser = session, username
	a.emit("ssh:status", map[string]string{"state": "connected", "user": username})
	level, _ := strconv.Atoi(strings.TrimPrefix(username, "bandit"))
	a.sshKeyInput = keyInputGuard{}
	if known, ok := knownBanditPassword(level); ok && authenticatedPassword == known {
		if _, unlocked := a.unlockedKeys[level]; !unlocked {
			a.unlockVerifiedKeyLocked(level, known)
		}
	}
	return nil
}

func (a *App) newKeyMatcher(level int) *keyOutputMatcher {
	a.mu.Lock()
	defer a.mu.Unlock()
	return newKeyOutputMatcher(level, a.keysEpoch)
}

func (a *App) matchKeyOutputLocked(m *keyOutputMatcher, chunk string, session *BanditSession, generation uint64) {
	if m.epoch != a.keysEpoch {
		*m = *newKeyOutputMatcher(m.level, a.keysEpoch)
	}
	if chunk == "" {
		chunk = "\n"
	}
	plain, candidates := m.candidates(chunk)
	if m.consume(plain) && (session == nil || !a.sshKeyInput.blocks(m.expected)) {
		a.unlockKeyLocked(m.level)
	}
	for _, password := range candidates {
		if password != m.expected {
			a.queueKeyVerificationLocked(keyVerification{level: m.level, password: password, epoch: a.keysEpoch, session: session, generation: generation})
		}
	}
}

func (a *App) emitSSHKeyOutput(session *BanditSession, m *keyOutputMatcher, chunk string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.keysClosed || a.sshSession != session {
		return
	}
	if chunk != "" {
		a.emit("ssh:data", chunk)
	}
	a.matchKeyOutputLocked(m, chunk, session, 0)
}

func (a *App) emitGitKeyOutput(m *keyOutputMatcher, generation uint64, chunk string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.keysClosed || a.gitOutputGeneration != generation {
		return
	}
	if chunk != "" {
		a.emit("git:output", chunk)
	}
	if m.level >= 28 && m.level <= 32 {
		a.matchKeyOutputLocked(m, chunk, nil, generation)
	}
}

type keyVerification struct {
	level      int
	password   string
	epoch      uint64
	session    *BanditSession
	generation uint64
}

type keyVerificationState struct {
	nextAttempt time.Time
	running     bool
	pending     *keyVerification
	noisy       bool
	cancel      context.CancelFunc
	last        [34][32]byte
	attempted   [34]time.Time
}

func (a *App) cancelKeyVerificationLocked() {
	a.keyVerification.pending = nil
	a.keyVerification.noisy = false
	a.keyVerification.nextAttempt = time.Time{}
	a.keyVerification.last = [34][32]byte{}
	a.keyVerification.attempted = [34]time.Time{}
	if a.keyVerification.cancel != nil {
		a.keyVerification.cancel()
	}
}

func (a *App) keyVerificationCurrentLocked(v keyVerification) bool {
	if a.keysClosed || v.epoch != a.keysEpoch {
		return false
	}
	if v.session != nil {
		return a.sshSession == v.session && !a.sshKeyInput.blocks(v.password)
	}
	return a.gitOutputGeneration == v.generation
}

func (a *App) queueKeyVerificationLocked(v keyVerification) {
	if v.level < 1 || v.level > 33 || !a.keyVerificationCurrentLocked(v) {
		return
	}
	state := &a.keyVerification
	hash := sha256.Sum256([]byte(v.password))
	if state.last[v.level] == hash && time.Since(state.attempted[v.level]) < 15*time.Second {
		return
	}
	if state.pending == nil && !state.noisy {
		state.pending = &v
	} else if state.pending != nil && *state.pending != v {
		state.pending = nil
		state.noisy = true
	}
	if !state.running {
		state.running = true
		go a.verifyKeyOutput()
	}
}

// One worker and one candidate; ambiguous output batches never generate authentication bursts.
func (a *App) verifyKeyOutput() {
	a.mu.Lock()
	delay := time.Until(a.keyVerification.nextAttempt)
	a.mu.Unlock()
	if delay < 250*time.Millisecond {
		delay = 250 * time.Millisecond
	}
	for {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-a.appContext().Done():
			timer.Stop()
			a.mu.Lock()
			a.keyVerification.running = false
			a.cancelKeyVerificationLocked()
			a.mu.Unlock()
			return
		}
		a.mu.Lock()
		state := &a.keyVerification
		v, noisy := state.pending, state.noisy
		state.pending, state.noisy = nil, false
		if v == nil || noisy || !a.keyVerificationCurrentLocked(*v) {
			state.running = false
			a.mu.Unlock()
			return
		}
		state.nextAttempt = time.Now().Add(2 * time.Second)
		state.last[v.level], state.attempted[v.level] = sha256.Sum256([]byte(v.password)), time.Now()
		ctx, cancel := context.WithTimeout(a.appContext(), 8*time.Second)
		state.cancel = cancel
		verify := a.verifyBanditPassword
		if verify == nil {
			verify = VerifyBanditPassword
		}
		a.mu.Unlock()
		err := verify(ctx, fmt.Sprintf("bandit%d", v.level), v.password, a.knownHosts)
		a.mu.Lock()
		if err == nil && ctx.Err() == nil && a.keyVerificationCurrentLocked(*v) {
			a.unlockVerifiedKeyLocked(v.level, v.password)
		}
		cancel()
		state.cancel = nil
		if state.pending == nil && !state.noisy {
			state.running = false
			a.mu.Unlock()
			return
		}
		delay = time.Until(state.nextAttempt)
		if delay < 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
		a.mu.Unlock()
	}
}

var banditPasswords = [...]string{
	"bandit0",
	"ZjLjTmM6FvvyRnrb2rfNWOZOTa6ip5If",
	"263JGJPfgU6LtdEvgfWU1XP5yac29mFx",
	"MNk8KNH3Usiio41PRUEoDFPqfxLPlSmx",
	"2WmrDFRmJIq3IPxneAaMGhap0pFhF3NJ",
	"4oQYVPkxZOOEOO5pTW81FB8j8lxXGUQw",
	"HWasnPhtq9AVKe0dmk45nxy20cvUa6EG",
	"morbNTDkSW6jIlUc0ymOdMaLnOlFVAaj",
	"dfwvzFQi4mU0wfNbFOe9RoWskMLg7eEc",
	"4CKMh1JI91bUIZZPXDqGanal4xvAg0JM",
	"FGUW5ilLVJrxX9kMYMmlN4MgbpfMiqey",
	"dtR173fZKb0RRsDFSGsg2RWnpNVj3qRr",
	"7x16WNeHIi5YkIhWsfFIqoognUTyj9Q4",
	"FO5dwFsc0cbaIiH0h8J2eUks2vdTDwAn",
	"MU4VWeTyJk8ROof1qqmcBPaLh7lDCPvS",
	"8xCjnmgoKbGLhHFAZlGE5Tmu4M2tKJQo",
	"kSkvUpMQ7lBYyCM4GBPvCvT1BfWRy0Dx",
	"EReVavePLFHtFlFsjn3hyzMlvSuSAcRD",
	"x2gLTTjFwMOhQ8oWNbMN362QKxfRqGlO",
	"cGWpMaKXVwDUNgPAVJbWYuGHVn9zl3j8",
	"0qXahG8ZjOVMN9Ghs7iOWsCfZyXOUbYO",
	"EeoULMCra2q0dSkYj561DX7s1CpBuOBt",
	"tRae0UfB9v0UzbCdn9cY0gQnds9GF58Q",
	"0Zf11ioIjMVN551jX3CmStKLYqjk54Ga",
	"gb8KRRCsshuZXI0tUuR6ypOFjiZbf3G8",
	"iCi86ttT4KSNe1armKiwbQNmB3YJP3q4",
	"s0773xxkk0MXfdqOfPRVr9L3jJBUOgCZ",
	"upsNCc7vzaRDx6oZC6GiR6ERwe1MowGB",
	"Yz9IpL0sBcCeuG7m9uQFt8ZNpS4HZRcN",
	"4pT1t5pnCCF961S7f362S2B0N84E952a",
	"qp30ex3VLz5MDG1n91Y224Q8n70p0j9B",
	"fb5S2xb7bRyFmAvQYQGEqsbhVyJqhnDy",
	"3O9RfhqyAlVBEZpVb6LYStshZoqoSx5K",
	"tQdtbs5D5i2vJwkO8mEyYEyTL8izoeJ0",
}
