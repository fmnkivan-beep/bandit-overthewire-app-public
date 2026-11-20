package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	gitAskpassTokenEnv = "BANDIT_ASKPASS_TOKEN"
	gitAskpassURLEnv   = "BANDIT_ASKPASS_URL"
	gitAskpassHeader   = "X-Bandit-Askpass-Token"
)

func runGitAskpass() bool {
	token, endpoint := os.Getenv(gitAskpassTokenEnv), os.Getenv(gitAskpassURLEnv)
	if token == "" || endpoint == "" {
		return false
	}
	_ = os.Unsetenv(gitAskpassTokenEnv)
	_ = os.Unsetenv(gitAskpassURLEnv)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Path != "/internal/askpass" {
		os.Exit(1)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		os.Exit(1)
	}
	req.Header.Set(gitAskpassHeader, token)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	response, err := client.Do(req)
	if err != nil {
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	password, err := io.ReadAll(io.LimitReader(response.Body, 257))
	if err != nil || len(password) == 0 || len(password) > 256 {
		clear(password)
		os.Exit(1)
	}
	defer clear(password)
	_, _ = os.Stdout.Write(append(password, '\n'))
	return true
}

func (a *App) startGitAskpass(password string) (string, func(), error) {
	if a.askpassEndpoint == "" || password == "" || len(password) > 256 {
		return "", nil, errors.New("не удалось подготовить одноразовый запрос пароля")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	token := hex.EncodeToString(raw[:])
	secret := []byte(password)
	a.mu.Lock()
	a.askpass[token] = secret
	a.mu.Unlock()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			a.mu.Lock()
			clear(a.askpass[token])
			delete(a.askpass, token)
			a.mu.Unlock()
		})
	}
	return token, cleanup, nil
}

func (a *App) consumeGitAskpass(token string) ([]byte, bool) {
	if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
		return nil, false
	}
	a.mu.Lock()
	secret, ok := a.askpass[token]
	delete(a.askpass, token)
	if ok {
		password := append([]byte(nil), secret...)
		clear(secret)
		a.mu.Unlock()
		return password, true
	}
	a.mu.Unlock()
	return nil, false
}
