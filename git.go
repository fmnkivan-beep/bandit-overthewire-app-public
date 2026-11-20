package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errInvalidGitCommand = errors.New("команда Git не разрешена для этого этапа")

// GitCommand содержит безопасно разобранное имя действия и аргументы для argv.
// Выполнять эти аргументы следует напрямую через exec.CommandContext, не через shell.
type GitCommand struct {
	Name string
	Args []string
}

// GitRepoURL возвращает единственный SSH-адрес репозитория для выбранного Git-этапа.
func GitRepoURL(levelID int) string {
	if levelID < 28 || levelID > 32 {
		return ""
	}
	user := fmt.Sprintf("bandit%d-git", levelID-1)
	return fmt.Sprintf("ssh://%s@bandit.labs.overthewire.org:2220/home/%s/repo", user, user)
}

// ParseGitCommand проверяет ввод и возвращает только одну разрешённую операцию.
func ParseGitCommand(levelID int, line string) (GitCommand, error) {
	if GitRepoURL(levelID) == "" || len(line) == 0 || len(line) > 4096 {
		return GitCommand{}, errInvalidGitCommand
	}
	words, err := splitGitLine(line)
	if err != nil || len(words) == 0 {
		return GitCommand{}, errInvalidGitCommand
	}
	name := words[0]
	args := words[1:]
	if name == "git" {
		if len(args) == 0 {
			return GitCommand{}, errInvalidGitCommand
		}
		name, args = args[0], args[1:]
	}
	if name == "clone" && len(args) == 0 {
		args = []string{GitRepoURL(levelID)}
	}
	if !validGitOperation(levelID, name, args) {
		return GitCommand{}, errInvalidGitCommand
	}
	return GitCommand{Name: name, Args: append([]string(nil), args...)}, nil
}

func validGitOperation(levelID int, name string, args []string) bool {
	switch name {
	case "clone":
		if len(args) < 1 || len(args) > 2 || args[0] != GitRepoURL(levelID) {
			return false
		}
		return len(args) == 1 || safeRepoName(args[1])
	case "status":
		return onlyArgs(args, nil, "--short", "--branch", "-sb", "-s")
	case "log":
		return onlyArgs(args, nil, "-p", "--oneline", "--all", "--decorate", "--stat")
	case "show":
		return len(args) == 0 || len(args) == 1 && safeGitRef(args[0])
	case "branch":
		return onlyArgs(args, nil, "-a", "--all", "-r", "--remotes", "-v", "--verbose")
	case "checkout":
		return levelID == 30 && len(args) == 1 && safeGitRef(args[0])
	case "tag":
		if len(args) == 0 {
			return true
		}
		return len(args) == 1 && (args[0] == "-l" || args[0] == "--list")
	case "add":
		if levelID != 32 || len(args) == 0 {
			return false
		}
		if args[0] == "-f" || args[0] == "--force" {
			args = args[1:]
		}
		if len(args) == 0 {
			return false
		}
		for _, path := range args {
			if !safeRepoPath(path) {
				return false
			}
		}
		return true
	case "commit":
		return levelID == 32 && len(args) == 2 && (args[0] == "-m" || args[0] == "--message") && len(args[1]) > 0 && len(args[1]) <= 512
	case "push":
		return levelID == 32 && len(args) == 2 && args[0] == "origin" && safeGitRef(args[1])
	case "ls":
		return len(args) == 0 || len(args) == 1 && safeRepoPath(args[0])
	case "cat":
		return len(args) == 1 && safeRepoPath(args[0])
	case "write":
		return levelID == 32 && len(args) == 2 && safeRepoPath(args[0]) && len(args[1]) <= 4096
	default:
		return false
	}
}

func onlyArgs(args []string, required []string, optional ...string) bool {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, arg := range append(required, optional...) {
		allowed[arg] = true
	}
	seen := make(map[string]bool, len(args))
	for _, arg := range args {
		if !allowed[arg] || seen[arg] {
			return false
		}
		seen[arg] = true
	}
	for _, arg := range required {
		if !seen[arg] {
			return false
		}
	}
	return true
}

func safeRepoName(name string) bool {
	return name != "." && safeGitRef(name) && !strings.ContainsAny(name, "/\\:") && !strings.EqualFold(name, ".git")
}

func safeGitRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || strings.ContainsAny(ref, "~^:?*[\\\\") {
		return false
	}
	for _, r := range ref {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)) {
			return false
		}
	}
	return true
}

func safeRepoPath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") || strings.HasPrefix(part, "-") {
			return false
		}
		if strings.ContainsAny(part, "\x00\r\n") {
			return false
		}
	}
	return true
}

// splitGitLine tokenizes quoted arguments without invoking a command shell.
func splitGitLine(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range line {
		if r == 0 || r == '\n' || r == '\r' || r < 0x20 && r != '\t' {
			return nil, errInvalidGitCommand
		}
		if escaped {
			word.WriteRune(r)
			escaped, started = false, true
			continue
		}
		if quote == '"' && r == '\\' {
			escaped, started = true, true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote, started = r, true
			continue
		}
		if strings.ContainsRune(";|&><`$", r) {
			return nil, errInvalidGitCommand
		}
		if r == ' ' || r == '\t' {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 || escaped {
		return nil, errInvalidGitCommand
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

// ResolveGitPath resolves a user path inside the selected repository and rejects symlink escapes.
func ResolveGitPath(workspaceRoot, repository, userPath string) (string, error) {
	if !safeRepoName(repository) || !safeRepoPath(userPath) {
		return "", errInvalidGitCommand
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", err
	}
	repo := filepath.Join(root, repository)
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	repoReal, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", err
	}
	if !insidePath(rootReal, repoReal) || rootReal == repoReal {
		return "", errInvalidGitCommand
	}
	target := filepath.Join(repoReal, filepath.FromSlash(userPath))
	ancestor := target
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			if !insidePath(repoReal, resolved) {
				return "", errInvalidGitCommand
			}
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errInvalidGitCommand
		}
		ancestor = parent
	}
	return target, nil
}

func insidePath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
