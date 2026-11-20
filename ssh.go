package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const BanditHost = "bandit.labs.overthewire.org"

var banditUsernamePattern = regexp.MustCompile(`^bandit(?:[0-9]|[12][0-9]|3[0-3])$`)

func ValidateBanditUsername(username string) error {
	if !banditUsernamePattern.MatchString(username) {
		return fmt.Errorf("неверное имя пользователя Bandit")
	}
	return nil
}

type HostKeyPrompt struct {
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"keyType"`
}

type HostKeyPromptError struct {
	Prompt HostKeyPrompt
}

func (e *HostKeyPromptError) Error() string {
	return "ключ SSH-сервера ещё не подтверждён"
}

func NewBanditHostKeyCallback(knownHostsPath string, confirm func(HostKeyPrompt) bool) (ssh.HostKeyCallback, error) {
	if strings.TrimSpace(knownHostsPath) == "" {
		return nil, errors.New("не задан файл доверенных ключей SSH")
	}
	address := net.JoinHostPort(BanditHost, "2220")
	var mu sync.Mutex
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if hostname != address {
			return fmt.Errorf("SSH-подключение к %q запрещено", hostname)
		}
		if key == nil {
			return errors.New("сервер прислал пустой ключ SSH")
		}
		mu.Lock()
		defer mu.Unlock()

		verify, exists, err := knownHostVerifier(knownHostsPath)
		if err != nil {
			return err
		}
		if exists {
			err = verify(hostname, remote, key)
			if err == nil {
				return nil
			}
			var keyErr *knownhosts.KeyError
			if !errors.As(err, &keyErr) {
				return fmt.Errorf("проверка ключа SSH: %w", err)
			}
			if len(keyErr.Want) > 0 {
				return errors.New("ключ SSH-сервера изменился; подключение отклонено")
			}
		}

		prompt := HostKeyPrompt{Address: address, Fingerprint: ssh.FingerprintSHA256(key), KeyType: key.Type()}
		if confirm == nil || !confirm(prompt) {
			return &HostKeyPromptError{Prompt: prompt}
		}
		if err := saveKnownHost(knownHostsPath, key); err != nil {
			return fmt.Errorf("сохранить ключ SSH-сервера: %w", err)
		}
		return nil
	}, nil
}

func knownHostVerifier(path string) (ssh.HostKeyCallback, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	verify, err := knownhosts.New(path)
	if err != nil {
		return nil, false, err
	}
	return verify, true, nil
}

func saveKnownHost(path string, key ssh.PublicKey) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var existing []byte
	if data, err := os.ReadFile(path); err == nil {
		existing = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	line := knownhosts.Line([]string{"[" + BanditHost + "]:2220"}, key)
	contents := append(existing, []byte(line+"\n")...)
	tmp, err := os.CreateTemp(dir, ".known-hosts-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

type BanditSession struct {
	Client  *ssh.Client
	Session *ssh.Session
	Input   io.WriteCloser
	Output  io.Reader
	Errors  io.Reader
}

func ConnectBandit(ctx context.Context, username, password, knownHostsPath string, confirm func(HostKeyPrompt) bool) (*BanditSession, error) {
	if err := ValidateBanditUsername(username); err != nil {
		return nil, err
	}
	if password == "" {
		return nil, errors.New("введите пароль Bandit")
	}
	hostKeyCallback, err := NewBanditHostKeyCallback(knownHostsPath, confirm)
	if err != nil {
		return nil, err
	}
	address := net.JoinHostPort(BanditHost, "2220")
	config := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к серверу Bandit: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SSH-подключение не установлено: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, chans, reqs)
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("не удалось открыть терминал: %w", err)
	}
	input, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("не удалось настроить ввод терминала: %w", err)
	}
	output, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("не удалось настроить вывод терминала: %w", err)
	}
	errOutput, err := session.StderrPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("не удалось настроить поток ошибок терминала: %w", err)
	}
	if err := session.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{
		ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("сервер не принял запрос терминала: %w", err)
	}
	if err := session.Shell(); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("не удалось запустить оболочку Bandit: %w", err)
	}
	return &BanditSession{Client: client, Session: session, Input: input, Output: output, Errors: errOutput}, nil
}

func (s *BanditSession) Resize(cols, rows int) error {
	if s == nil || s.Session == nil {
		return errors.New("SSH-сеанс закрыт")
	}
	if cols < 1 || rows < 1 || cols > 500 || rows > 500 {
		return errors.New("неверный размер терминала")
	}
	return s.Session.WindowChange(rows, cols)
}

func (s *BanditSession) Wait() error {
	if s == nil || s.Session == nil {
		return errors.New("SSH-сеанс закрыт")
	}
	return s.Session.Wait()
}

func (s *BanditSession) Close() error {
	if s == nil {
		return nil
	}
	var sessionErr, clientErr error
	if s.Session != nil {
		sessionErr = s.Session.Close()
	}
	if s.Client != nil {
		clientErr = s.Client.Close()
	}
	if sessionErr != nil && !errors.Is(sessionErr, io.EOF) {
		return sessionErr
	}
	return clientErr
}

// VerifyBanditPassword authenticates only, using an already pinned host key. No session or PTY is opened.
func VerifyBanditPassword(ctx context.Context, username, password, knownHostsPath string) error {
	if err := ValidateBanditUsername(username); err != nil {
		return err
	}
	callback, err := NewBanditHostKeyCallback(knownHostsPath, nil)
	if err != nil {
		return err
	}
	address := net.JoinHostPort(BanditHost, "2220")
	conn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	client, _, _, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: username, Auth: []ssh.AuthMethod{ssh.Password(password)}, HostKeyCallback: callback, Timeout: 8 * time.Second})
	if err == nil {
		_ = client.Close()
	}
	return err
}
