package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitCommands(t *testing.T) {
	tests := []struct {
		name    string
		levelID int
		line    string
		command string
		args    []string
		ok      bool
	}{
		{"clone selected repository", 28, "git clone ssh://bandit27-git@bandit.labs.overthewire.org:2220/home/bandit27-git/repo", "clone", []string{"ssh://bandit27-git@bandit.labs.overthewire.org:2220/home/bandit27-git/repo"}, true},
		{"status", 28, "git status --short", "status", []string{"--short"}, true},
		{"history including deleted password", 29, "git log -p", "log", []string{"-p"}, true},
		{"commit message keeps spaces", 32, `git commit -m "add the key file"`, "commit", []string{"-m", "add the key file"}, true},
		{"commit message may contain shell punctuation as inert text", 32, `git commit -m 'push; this is a message'`, "commit", []string{"-m", "push; this is a message"}, true},
		{"force-add task file", 32, "git add -f key.txt", "add", []string{"-f", "key.txt"}, true},
		{"push task branch", 32, "git push origin master", "push", []string{"origin", "master"}, true},
		{"list files", 28, "ls", "ls", nil, true},
		{"read repo file", 28, "cat README", "cat", []string{"README"}, true},
		{"write file contents", 32, `write key.txt "May I come in?"`, "write", []string{"key.txt", "May I come in?"}, true},
		{"reject shell pipeline", 28, "git status | powershell", "", nil, false},
		{"reject command chaining", 28, "git status; whoami", "", nil, false},
		{"reject arbitrary executable", 28, "powershell -Command whoami", "", nil, false},
		{"reject unsupported subcommand", 28, "git reset --hard", "", nil, false},
		{"reject unsafe option", 28, "git push --force origin master", "", nil, false},
		{"reject clone for different level", 29, "git clone ssh://bandit27-git@bandit.labs.overthewire.org:2220/home/bandit27-git/repo", "", nil, false},
		{"reject option-looking file name", 28, "cat --help", "", nil, false},
		{"reject clone into workspace root", 28, "git clone ssh://bandit27-git@bandit.labs.overthewire.org:2220/home/bandit27-git/repo .", "", nil, false},
		{"reject control characters", 28, "git status\nwhoami", "", nil, false},
		{"reject unmatched quote", 32, `git commit -m "unfinished`, "", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseGitCommand(test.levelID, test.line)
			if !test.ok {
				if err == nil {
					t.Fatalf("ParseGitCommand(%q) succeeded, want rejection", test.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGitCommand(%q): %v", test.line, err)
			}
			if got.Name != test.command {
				t.Errorf("command = %q, want %q", got.Name, test.command)
			}
			if len(got.Args) != len(test.args) {
				t.Fatalf("args = %#v, want %#v", got.Args, test.args)
			}
			for i := range test.args {
				if got.Args[i] != test.args[i] {
					t.Errorf("args[%d] = %q, want %q", i, got.Args[i], test.args[i])
				}
			}
		})
	}
}

func TestGitRepoURLMatchesLevel(t *testing.T) {
	want := map[int]string{
		28: "ssh://bandit27-git@bandit.labs.overthewire.org:2220/home/bandit27-git/repo",
		29: "ssh://bandit28-git@bandit.labs.overthewire.org:2220/home/bandit28-git/repo",
		30: "ssh://bandit29-git@bandit.labs.overthewire.org:2220/home/bandit29-git/repo",
		31: "ssh://bandit30-git@bandit.labs.overthewire.org:2220/home/bandit30-git/repo",
		32: "ssh://bandit31-git@bandit.labs.overthewire.org:2220/home/bandit31-git/repo",
	}
	for level, url := range want {
		if got := GitRepoURL(level); got != url {
			t.Errorf("GitRepoURL(%d) = %q, want %q", level, got, url)
		}
	}
	for _, level := range []int{-1, 27, 33} {
		if got := GitRepoURL(level); got != "" {
			t.Errorf("GitRepoURL(%d) = %q, want empty for non-Git stage", level, got)
		}
	}
}

func TestGitCommitUsesTrainingIdentity(t *testing.T) {
	git, err := findGitExecutable()
	if err != nil {
		t.Skip("Git executable is unavailable")
	}
	app := NewApp(newEventHub(), "")
	app.workspace = t.TempDir()
	repo := filepath.Join(app.workspace, "level-32")
	init := exec.Command(git, "init", "--quiet", "--template=", repo)
	init.Env = safeGitEnvironment(app.workspace)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	t.Setenv("GIT_AUTHOR_NAME", "Host author")
	t.Setenv("GIT_AUTHOR_EMAIL", "host-author@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Host committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "host-committer@example.invalid")
	for _, command := range []string{`write key.txt "May I come in?"`, "git add -f key.txt", "git commit -m key"} {
		if err := app.RunGitCommand(32, command); err != nil {
			t.Fatalf("%q: %v", command, err)
		}
	}
	show := exec.Command(git, "show", "-s", "--format=%an%n%ae%n%cn%n%ce")
	show.Dir = repo
	show.Env = safeGitEnvironment(app.workspace)
	output, err := show.CombinedOutput()
	if err != nil {
		t.Fatalf("git show: %v: %s", err, output)
	}
	want := "Bandit 2 Trainer\nbandit2@example.invalid\nBandit 2 Trainer\nbandit2@example.invalid"
	if got := strings.TrimSpace(strings.ReplaceAll(string(output), "\r\n", "\n")); got != want {
		t.Fatalf("commit identity = %q, want %q", got, want)
	}
}

func TestResolveGitPathStaysInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "bandit")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"README.md", "src/key.txt", "notes with spaces.txt"} {
		got, err := ResolveGitPath(root, "bandit", rel)
		if err != nil {
			t.Errorf("ResolveGitPath(%q): %v", rel, err)
			continue
		}
		want := filepath.Join(repo, filepath.FromSlash(rel))
		if got != want {
			t.Errorf("ResolveGitPath(%q) = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{".", "../outside", "src/../../outside", filepath.Join(root, "outside"), `C:\outside`, `..\outside`, ".git/config", "src/.git/config"} {
		if got, err := ResolveGitPath(root, "bandit", rel); err == nil {
			t.Errorf("ResolveGitPath(%q) = %q, want rejection", rel, got)
		}
	}
}

func TestResolveGitPathRejectsRepositorySymlinkToWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(root, filepath.Join(root, "bandit")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if got, err := ResolveGitPath(root, "bandit", "outside-file"); err == nil {
		t.Errorf("ResolveGitPath through repo symlink to workspace root = %q, want rejection", got)
	}
}

func TestResolveGitPathRejectsSymlinksOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "bandit")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "outside-link")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if got, err := ResolveGitPath(root, "bandit", "outside-link/secret"); err == nil {
		t.Errorf("ResolveGitPath through an outside symlink = %q, want rejection", got)
	}
}
