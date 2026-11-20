package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

//go:embed levels.json
var levelData embed.FS

type Level struct {
	ID        int      `json:"id"`
	Code      string   `json:"code"`
	Title     string   `json:"title"`
	Goal      string   `json:"goal"`
	Commands  []string `json:"commands"`
	Hint      string   `json:"hint"`
	SourceURL string   `json:"sourceUrl"`
}

var levels []Level

func init() {
	data, err := levelData.ReadFile("levels.json")
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(data, &levels); err != nil {
		panic(err)
	}
}

type App struct {
	events          *eventHub
	ctx             context.Context
	askpassEndpoint string

	mu                   sync.Mutex
	gitCommandMu         sync.Mutex
	sshInputMu           sync.Mutex
	sshSession           *BanditSession
	sshKeyInput          keyInputGuard
	sshUser              string
	sshConnecting        bool
	sshConnectCancel     context.CancelFunc
	sshHostDecision      chan bool
	gitPassword          string
	gitPasswordLevel     int
	gitInput             io.WriteCloser
	gitCancel            context.CancelFunc
	gitRunning           bool
	workspace            string
	knownHosts           string
	storageDir           string
	progressFile         string
	keysFile             string
	storageReady         bool
	progress             savedProgress
	progressErr          error
	keysStoreErr         error
	askpass              map[string][]byte
	unlockedKeys         map[int]unlockedKey
	unsavedKeys          map[int]unlockedKey
	keysEpoch            uint64
	keysClosed           bool
	keyVerification      keyVerificationState
	verifyBanditPassword func(context.Context, string, string, string) error
	gitOutputGeneration  uint64
}

func NewApp(events *eventHub, askpassEndpoint string) *App {
	baseDir := os.Getenv("LOCALAPPDATA")
	if baseDir == "" {
		baseDir, _ = os.UserConfigDir()
	}
	return &App{
		events:          events,
		workspace:       filepath.Join(baseDir, "Bandit2", "workspace"),
		knownHosts:      filepath.Join(baseDir, "Bandit2", "known_hosts"),
		progress:        defaultProgress(),
		askpassEndpoint: askpassEndpoint,
		askpass:         make(map[string][]byte),
	}
}

func (a *App) Startup(ctx context.Context) error {
	a.ctx = ctx
	if err := os.MkdirAll(a.workspace, 0700); err != nil {
		return err
	}
	a.storageDir = filepath.Dir(a.workspace)
	a.progressFile = filepath.Join(a.storageDir, "progress.json")
	a.keysFile = filepath.Join(a.storageDir, "unlocked-keys.json")
	a.storageReady = true
	a.progress, a.progressErr = a.readProgress()
	if a.progressErr != nil {
		a.progress = defaultProgress()
	}
	keys, err := a.readUnlockedKeys()
	if err != nil {
		a.keysStoreErr = err
		return nil
	}
	a.unlockedKeys = make(map[int]unlockedKey, len(keys))
	a.unsavedKeys = make(map[int]unlockedKey)
	for _, key := range keys {
		a.unlockedKeys[key.Level] = key
	}
	return nil
}

func (a *App) Shutdown() error {
	a.mu.Lock()
	a.keysClosed = true
	a.sshKeyInput = keyInputGuard{}
	clear(a.unlockedKeys)
	a.keysEpoch++
	a.cancelKeyVerificationLocked()
	a.gitOutputGeneration++
	if a.gitCancel != nil {
		a.gitCancel()
	}
	if a.sshConnectCancel != nil {
		a.sshConnectCancel()
	}
	a.gitPassword = ""
	a.gitPasswordLevel = 0
	session := a.sshSession
	a.sshSession = nil
	for token, secret := range a.askpass {
		clear(secret)
		delete(a.askpass, token)
	}
	a.mu.Unlock()
	if session != nil {
		return session.Close()
	}
	return nil
}

func (a *App) GetLevels() []Level {
	result := make([]Level, len(levels))
	copy(result, levels)
	for i := range result {
		result[i].Commands = append([]string(nil), result[i].Commands...)
	}
	return result
}

func (a *App) SSHStatus() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sshConnecting {
		return map[string]string{"state": "connecting", "user": a.sshUser}
	}
	if a.sshSession != nil {
		return map[string]string{"state": "connected", "user": a.sshUser}
	}
	return map[string]string{"state": "disconnected"}
}

func (a *App) ConnectSSH(username, password string) error {
	if err := ValidateBanditUsername(username); err != nil {
		return err
	}
	if password == "" || len(password) > 256 {
		return errors.New("введите пароль Bandit")
	}
	a.mu.Lock()
	if a.sshSession != nil || a.sshConnecting {
		a.mu.Unlock()
		return errors.New("сначала отключись от текущего уровня")
	}
	ctx, cancel := context.WithTimeout(a.appContext(), 2*time.Minute)
	a.sshConnecting = true
	a.sshConnectCancel = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.sshConnecting = false
		a.sshConnectCancel = nil
		a.mu.Unlock()
	}()
	a.emit("ssh:status", map[string]string{"state": "connecting", "user": username})
	session, err := ConnectBandit(ctx, username, password, a.knownHosts, func(prompt HostKeyPrompt) bool {
		return a.askHostKey(ctx, prompt)
	})
	if err != nil {
		password = ""
		if errors.Is(err, context.Canceled) {
			a.emit("ssh:status", map[string]string{"state": "disconnected"})
		} else {
			a.emit("ssh:status", map[string]string{"state": "error", "message": err.Error()})
		}
		return err
	}
	if err := a.activateSSHSession(ctx, session, username, password); err != nil {
		_ = session.Close()
		return err
	}
	password = ""
	go a.streamSSH(session)
	return nil
}

func (a *App) askHostKey(ctx context.Context, prompt HostKeyPrompt) bool {
	decision := make(chan bool, 1)
	a.mu.Lock()
	if a.sshHostDecision != nil {
		a.mu.Unlock()
		return false
	}
	a.sshHostDecision = decision
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.sshHostDecision = nil
		a.mu.Unlock()
	}()
	a.emit("ssh:host-key", prompt)
	select {
	case accepted := <-decision:
		return accepted
	case <-ctx.Done():
		return false
	case <-time.After(2 * time.Minute):
		return false
	}
}

func (a *App) ConfirmHostKey(accept bool) {
	a.mu.Lock()
	decision := a.sshHostDecision
	a.mu.Unlock()
	if decision != nil {
		select {
		case decision <- accept:
		default:
		}
	}
}

func (a *App) streamSSH(session *BanditSession) {
	a.mu.Lock()
	level, _ := strconv.Atoi(strings.TrimPrefix(a.sshUser, "bandit"))
	active := a.sshSession == session
	epoch := a.keysEpoch
	a.mu.Unlock()
	if !active {
		return
	}
	var readers sync.WaitGroup
	stream := func(reader io.Reader) {
		defer readers.Done()
		matcher := newKeyOutputMatcher(level+1, epoch)
		buffer := make([]byte, 2048)
		for {
			n, err := reader.Read(buffer)
			if n > 0 {
				a.emitSSHKeyOutput(session, matcher, string(buffer[:n]))
			}
			if err != nil {
				if err == io.EOF {
					a.emitSSHKeyOutput(session, matcher, "")
				}
				return
			}
		}
	}
	readers.Add(2)
	go stream(session.Output)
	go stream(session.Errors)
	_ = session.Wait()
	_ = session.Close()
	readers.Wait()
	a.mu.Lock()
	wasCurrent := a.sshSession == session
	if wasCurrent {
		a.sshSession = nil
		a.cancelKeyVerificationLocked()
		a.sshUser = ""
		a.sshKeyInput = keyInputGuard{}
	}
	a.mu.Unlock()
	if wasCurrent {
		a.emit("ssh:status", map[string]string{"state": "disconnected"})
	}
}

func (a *App) SendSSHData(data string) error {
	if len(data) > 4096 {
		return errors.New("слишком большой ввод")
	}
	a.sshInputMu.Lock()
	defer a.sshInputMu.Unlock()
	a.mu.Lock()
	session := a.sshSession
	if session != nil {
		a.sshKeyInput.observe(data)
	}
	a.mu.Unlock()
	if session == nil {
		return errors.New("SSH-сеанс не подключён")
	}
	_, err := io.WriteString(session.Input, data)
	return err
}

func (a *App) ResizeSSH(cols, rows int) error {
	a.mu.Lock()
	session := a.sshSession
	a.mu.Unlock()
	if session == nil {
		return errors.New("SSH-сеанс не подключён")
	}
	return session.Resize(cols, rows)
}

func (a *App) DisconnectSSH() error {
	a.mu.Lock()
	if a.sshConnectCancel != nil {
		a.sshConnectCancel()
	}
	session := a.sshSession
	a.sshSession = nil
	a.cancelKeyVerificationLocked()
	a.sshUser = ""
	a.sshKeyInput = keyInputGuard{}
	connecting := a.sshConnecting
	a.mu.Unlock()
	if session == nil {
		if connecting {
			a.emit("ssh:status", map[string]string{"state": "disconnected"})
		}
		return nil
	}
	err := session.Close()
	a.emit("ssh:status", map[string]string{"state": "disconnected"})
	return err
}

func (a *App) RunGitCommand(levelID int, line string) error {
	a.gitCommandMu.Lock()
	defer a.gitCommandMu.Unlock()
	parsed, err := ParseGitCommand(levelID, line)
	if err != nil {
		return err
	}
	if levelID < 28 || levelID > 32 {
		return errors.New("локальный Git доступен только на пяти специальных этапах")
	}
	if parsed.Name == "ls" || parsed.Name == "cat" || parsed.Name == "write" {
		return a.runGitFileCommand(levelID, parsed)
	}
	if parsed.Name == "clone" || parsed.Name == "push" {
		if err := a.ensureGitHostKey(); err != nil {
			return err
		}
	}
	gitExe, err := findGitExecutable()
	if err != nil {
		return errors.New("Git for Windows не найден. Установи Git и перезапусти приложение.")
	}
	repo := filepath.Join(a.workspace, fmt.Sprintf("level-%d", levelID))
	workingDir := a.workspace
	args := append([]string{parsed.Name}, parsed.Args...)
	if parsed.Name == "clone" {
		if err := os.MkdirAll(a.workspace, 0700); err != nil {
			return err
		}
		if _, err := os.Stat(repo); err == nil {
			return errors.New("репозиторий этого этапа уже есть в рабочей папке")
		}
		args = append(args, filepath.Base(repo))
	} else {
		if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
			return errors.New("сначала клонируй репозиторий этого этапа")
		}
		workingDir = repo
	}
	return a.runGitProcess(gitExe, args, workingDir, levelID)
}

func (a *App) runGitFileCommand(levelID int, parsed GitCommand) error {
	matcher := a.newKeyMatcher(levelID)
	a.mu.Lock()
	generation := a.gitOutputGeneration
	a.mu.Unlock()
	output := func(chunk string) { a.emitGitKeyOutput(matcher, generation, chunk) }
	repoName := fmt.Sprintf("level-%d", levelID)
	repo := filepath.Join(a.workspace, repoName)
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		return errors.New("сначала клонируй репозиторий этого этапа")
	}
	switch parsed.Name {
	case "ls":
		path := repo
		if len(parsed.Args) == 1 {
			var err error
			path, err = ResolveGitPath(a.workspace, repoName, parsed.Args[0])
			if err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		var names []string
		for _, entry := range entries {
			if entry.Name() != ".git" {
				names = append(names, entry.Name())
			}
		}
		output(strings.Join(names, "\r\n") + "\r\n")
	case "cat":
		path, err := ResolveGitPath(a.workspace, repoName, parsed.Args[0])
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
		if err != nil {
			return err
		}
		if len(data) > 1024*1024 {
			return errors.New("файл слишком большой для вывода в терминал")
		}
		output(string(data))
		output("")
	case "write":
		path, err := ResolveGitPath(a.workspace, repoName, parsed.Args[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(parsed.Args[1]), 0600); err != nil {
			return err
		}
		output("Файл записан.\r\n")
	}
	return nil
}

func (a *App) runGitProcess(gitExe string, args []string, dir string, levelID int) error {
	ctx, cancel := context.WithTimeout(a.appContext(), 3*time.Minute)
	a.mu.Lock()
	if a.gitRunning {
		a.mu.Unlock()
		cancel()
		return errors.New("команда Git уже выполняется")
	}
	a.gitRunning = true
	a.gitCancel = cancel
	a.gitOutputGeneration++
	generation := a.gitOutputGeneration
	epoch := a.keysEpoch
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.gitInput = nil
		a.gitCancel = nil
		a.gitRunning = false
		a.mu.Unlock()
	}()
	var askpassToken string
	if args[0] == "clone" || args[0] == "push" {
		a.mu.Lock()
		password, passwordLevel := a.gitPassword, a.gitPasswordLevel
		a.mu.Unlock()
		if password == "" || passwordLevel != levelID {
			return errors.New("введи пароль Git для выбранного этапа")
		}
		token, stopAskpass, err := a.startGitAskpass(password)
		if err != nil {
			return fmt.Errorf("не удалось подготовить SSH-пароль для Git: %w", err)
		}
		askpassToken = token
		defer stopAskpass()
	}
	appPath, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, gitExe, args...)
	cmd.Dir = dir
	cmd.Env = safeGitEnvironment(a.workspace)
	if args[0] == "commit" {
		const name = "Bandit 2 Trainer"
		const email = "bandit2@example.invalid"
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email,
			"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email,
		)
	}
	sshExe := "ssh"
	for _, candidate := range []string{
		filepath.Join(filepath.Dir(gitExe), "..", "usr", "bin", "ssh.exe"),
		filepath.Join(filepath.Dir(gitExe), "..", "..", "usr", "bin", "ssh.exe"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			sshExe = strconv.Quote(filepath.Clean(candidate))
			break
		}
	}
	sshCommand := sshExe + " -F NUL -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=NUL -o UserKnownHostsFile=" + strconv.Quote(a.knownHosts) + " -o IdentityAgent=none -o PreferredAuthentications=password -o PubkeyAuthentication=no -o NumberOfPasswordPrompts=1 -p 2220"
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=NUL",
		"GIT_CONFIG_KEY_2=core.pager", "GIT_CONFIG_VALUE_2=cat",
		"GIT_CONFIG_KEY_3=credential.interactive", "GIT_CONFIG_VALUE_3=never",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat", "GIT_SSH_VARIANT=ssh", "GIT_SSH_COMMAND="+sshCommand,
	)
	if askpassToken != "" {
		cmd.Env = append(cmd.Env,
			"SSH_ASKPASS="+appPath,
			"SSH_ASKPASS_REQUIRE=force",
			"DISPLAY=Bandit2",
			gitAskpassURLEnv+"="+a.askpassEndpoint,
			gitAskpassTokenEnv+"="+askpassToken,
		)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	a.mu.Lock()
	a.gitInput = stdin
	a.mu.Unlock()
	a.emit("git:status", map[string]string{"state": "running", "command": args[0], "level": fmt.Sprint(levelID)})
	var readers sync.WaitGroup
	stream := func(r io.Reader) {
		defer readers.Done()
		matcher := newKeyOutputMatcher(levelID, epoch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			a.emitGitKeyOutput(matcher, generation, scanner.Text()+"\r\n")
		}
		if err := scanner.Err(); err != nil {
			a.emit("git:output", "\r\nОшибка чтения вывода: "+err.Error()+"\r\n")
		}
	}
	readers.Add(2)
	go stream(stdout)
	go stream(stderr)
	waitErr := cmd.Wait()
	readers.Wait()
	stdin.Close()
	if waitErr != nil {
		a.emit("git:status", map[string]string{"state": "error", "message": waitErr.Error()})
		return fmt.Errorf("Git завершил команду с ошибкой: %w", waitErr)
	}
	a.emit("git:status", map[string]string{"state": "finished"})
	return nil
}

func safeGitEnvironment(workspace string) []string {
	env := []string{}
	for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env,
		"HOME="+workspace,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=NUL",
		"GIT_OPTIONAL_LOCKS=0",
	)
	return env
}

func findGitExecutable() (string, error) {
	for _, name := range []string{"git.exe", "git"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
		if root == "" {
			continue
		}
		installDir := "Git"
		if root == os.Getenv("LOCALAPPDATA") {
			installDir = filepath.Join("Programs", "Git")
		}
		path := filepath.Join(root, installDir, "cmd", "git.exe")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", errors.New("Git was not found")
}

func (a *App) SetGitPassword(levelID int, password string) error {
	if levelID < 28 || levelID > 32 || password == "" || len(password) > 256 {
		return errors.New("введи пароль Git для выбранного этапа")
	}
	a.mu.Lock()
	a.gitPassword = password
	a.gitPasswordLevel = levelID
	a.mu.Unlock()
	return nil
}

func (a *App) ClearGitPassword() {
	a.mu.Lock()
	a.gitPassword = ""
	a.gitPasswordLevel = 0
	a.mu.Unlock()
}

func (a *App) DisconnectGit() {
	a.mu.Lock()
	a.gitOutputGeneration++
	a.cancelKeyVerificationLocked()
	cancel, input := a.gitCancel, a.gitInput
	a.gitPassword = ""
	a.gitPasswordLevel = 0
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if input != nil {
		_ = input.Close()
	}
	a.emit("git:status", map[string]string{"state": "disconnected"})
}

func (a *App) SendGitData(data string) error {
	if len(data) > 4096 {
		return errors.New("слишком большой ввод")
	}
	a.mu.Lock()
	input := a.gitInput
	a.mu.Unlock()
	if input == nil {
		return errors.New("команда Git сейчас не ожидает ввода")
	}
	_, err := io.WriteString(input, data)
	return err
}

func (a *App) ensureGitHostKey() error {
	ctx, cancel := context.WithTimeout(a.appContext(), 2*time.Minute)
	defer cancel()
	callback, err := NewBanditHostKeyCallback(a.knownHosts, func(prompt HostKeyPrompt) bool {
		return a.askHostKey(ctx, prompt)
	})
	if err != nil {
		return err
	}
	verified := false
	hostKey := func(host string, remote net.Addr, key ssh.PublicKey) error {
		if err := callback(host, remote, key); err != nil {
			return err
		}
		verified = true
		return nil
	}
	address := net.JoinHostPort(BanditHost, "2220")
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("не удалось проверить сервер Bandit: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	_, _, _, authErr := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: "bandit27-git", HostKeyCallback: hostKey, Timeout: 10 * time.Second})
	if !verified {
		return fmt.Errorf("проверка ключа сервера не завершена: %w", authErr)
	}
	var serverAuth *ssh.ServerAuthError
	if authErr != nil && !errors.As(authErr, &serverAuth) {
		return fmt.Errorf("проверка сервера Bandit не удалась: %w", authErr)
	}
	return nil
}

func (a *App) appContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) emit(name string, data any) {
	if a.events != nil {
		a.events.publish(name, data)
	}
}
