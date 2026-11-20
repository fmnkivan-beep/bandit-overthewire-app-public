package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed frontend/dist
var frontendBundle embed.FS

func main() {
	if runGitAskpass() {
		return
	}
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("не удалось запустить локальный сервер: %w", err)
	}
	defer listener.Close()
	host := listener.Addr().String()
	endpoint := "http://" + host + "/internal/askpass"
	events := newEventHub()
	app := NewApp(events, endpoint)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Startup(ctx); err != nil {
		return err
	}
	defer app.Shutdown()
	server := &http.Server{
		Handler:           newHTTPHandler(app, events, frontendBundle, host),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	browser, err := findBrowser()
	if err != nil {
		return err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	profile := filepath.Join(configDir, "Bandit2", "browser-profile")
	if err := os.MkdirAll(profile, 0700); err != nil {
		return err
	}
	window := exec.Command(browser, "--app=http://"+host+"/", "--user-data-dir="+profile, "--window-size=1280,800", "--no-first-run", "--no-default-browser-check")
	if err := window.Start(); err != nil {
		return fmt.Errorf("не удалось открыть окно приложения: %w", err)
	}
	if err := window.Process.Release(); err != nil {
		return fmt.Errorf("не удалось запустить окно приложения: %w", err)
	}
	readyTimer := time.NewTimer(20 * time.Second)
	defer readyTimer.Stop()
	for !events.hasSubscribers() {
		select {
		case err := <-serveResult:
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("локальный сервер остановился: %w", err)
			}
			return nil
		case <-ctx.Done():
			return nil
		case <-readyTimer.C:
			return errors.New("Edge/Chrome не подключился к приложению за 20 секунд")
		case <-time.After(100 * time.Millisecond):
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var disconnectedAt time.Time
	for running := true; running; {
		select {
		case err := <-serveResult:
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("локальный сервер остановился: %w", err)
			}
			running = false
		case <-ctx.Done():
			running = false
		case <-ticker.C:
			if events.hasSubscribers() {
				disconnectedAt = time.Time{}
			} else if disconnectedAt.IsZero() {
				disconnectedAt = time.Now()
			} else if time.Since(disconnectedAt) >= 12*time.Second {
				running = false
			}
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
	}
	return nil
}

func findBrowser() (string, error) {
	for _, candidate := range []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("установите Microsoft Edge или Google Chrome для запуска окна BANDIT")
}
