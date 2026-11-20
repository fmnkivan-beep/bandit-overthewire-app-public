package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var errStaleProgress = errors.New("версия прогресса устарела")
var errResetBusy = errors.New("сначала отключитесь от SSH и дождитесь завершения Git")

const (
	progressVersion = 1
	progressFileMax = 8 << 10
	keysFileMax     = 16 << 10
	maxProgressRev  = uint64(9007199254740991)
)

type savedProgress struct {
	Version       int             `json:"version"`
	ActiveLevelID string          `json:"activeLevelId"`
	Completed     map[string]bool `json:"completed"`
	Revision      uint64          `json:"revision"`
}

type savedKeys struct {
	Version  int           `json:"version"`
	Unlocked []unlockedKey `json:"unlocked"`
	Revision uint64        `json:"revision"`
}

func defaultProgress() savedProgress {
	return savedProgress{Version: progressVersion, ActiveLevelID: "0", Completed: map[string]bool{"0": false}}
}

func validLevelID(id string) bool {
	if id == "0" {
		return true
	}
	fromText, toText, ok := strings.Cut(id, "-")
	if !ok {
		return false
	}
	from, err1 := strconv.Atoi(fromText)
	to, err2 := strconv.Atoi(toText)
	return err1 == nil && err2 == nil && from >= 0 && from <= 33 && to == from+1 && strconv.Itoa(from)+"-"+strconv.Itoa(to) == id
}

func validateProgress(value savedProgress) error {
	if value.Version != progressVersion || value.Revision > maxProgressRev || !validLevelID(value.ActiveLevelID) || value.Completed == nil || len(value.Completed) > 35 {
		return errors.New("неверный формат прогресса")
	}
	for id := range value.Completed {
		if !validLevelID(id) {
			return errors.New("неверный формат прогресса")
		}
	}
	return nil
}

func validUnlockedKey(key unlockedKey) bool {
	return key.Level >= 0 && key.Level <= 33 && key.Username == fmt.Sprintf("bandit%d", key.Level) &&
		key.Password != "" && len(key.Password) <= 256 && utf8.ValidString(key.Password) &&
		!containsControl(key.Password)
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r >= 0x7f && r <= 0x9f || r == 0x2028 || r == 0x2029 {
			return true
		}
	}
	return false
}

func decodeStored(path string, max int64, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return errors.New("файл сохранения имеет неверный тип или размер")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	if err := validateStoredFieldNames(data, target); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("лишние данные в файле сохранения")
		}
		return err
	}
	return nil
}

func decodeProgressJSON(w http.ResponseWriter, r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("ожидается JSON")
	}
	r.Body = http.MaxBytesReader(w, r.Body, progressFileMax)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return err
	}
	if err := validateExactJSONFields(body, "version", "activeLevelId", "completed", "revision"); err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return decodeJSON(w, r, target, progressFileMax)
}

func validateStoredFieldNames(data []byte, target any) error {
	switch target.(type) {
	case *savedProgress:
		return validateExactJSONFields(data, "version", "activeLevelId", "completed", "revision")
	case *savedKeys:
		if err := validateExactJSONFields(data, "version", "unlocked", "revision"); err != nil {
			return err
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(data, &root); err != nil {
			return err
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(root["unlocked"], &entries); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := validateExactJSONFields(entry, "level", "username", "password"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateExactJSONFields(data []byte, allowed ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("неверная схема JSON")
	}
	for name := range fields {
		found := false
		for _, expected := range allowed {
			if name == expected {
				found = true
				break
			}
		}
		if !found {
			return errors.New("неизвестное поле JSON")
		}
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := checkUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("лишние данные в JSON")
		}
		return err
	}
	return nil
}

func checkUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("повторяющееся поле JSON")
			}
			seen[name] = true
			if err := checkUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := checkUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("неверный JSON")
	}
}

func readProgressFile(path string) (savedProgress, error) {
	value := defaultProgress()
	if err := decodeStored(path, progressFileMax, &value); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultProgress(), nil
		}
		return savedProgress{}, err
	}
	if err := validateProgress(value); err != nil {
		return savedProgress{}, err
	}
	return value, nil
}

func readKeysFileWithRevision(path string) (savedKeys, error) {
	var value savedKeys
	if err := decodeStored(path, keysFileMax, &value); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return savedKeys{Version: progressVersion, Unlocked: []unlockedKey{}}, nil
		}
		return savedKeys{}, err
	}
	if value.Version != progressVersion || value.Unlocked == nil || len(value.Unlocked) > 34 {
		return savedKeys{}, errors.New("неверный формат архива ключей")
	}
	seen := make(map[int]bool, len(value.Unlocked))
	for _, key := range value.Unlocked {
		if !validUnlockedKey(key) || seen[key.Level] {
			return savedKeys{}, errors.New("неверный формат архива ключей")
		}
		seen[key.Level] = true
	}
	return value, nil
}

func readKeysFile(path string) ([]unlockedKey, error) {
	value, err := readKeysFileWithRevision(path)
	return value.Unlocked, err
}

func writeStoredAtomic(path string, value any, max int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > max {
		return errors.New("сохранение превышает допустимый размер")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("файл сохранения имеет неверный тип")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bandit-progress-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceStoredFile(tmpPath, path)
}

func mergeCompleted(previous, next map[string]bool) map[string]bool {
	merged := make(map[string]bool, len(previous)+len(next))
	for id, done := range previous {
		if done {
			merged[id] = true
		}
	}
	for id, done := range next {
		if done || !merged[id] {
			merged[id] = done
		}
	}
	return merged
}

func (a *App) readProgress() (savedProgress, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.storageReady {
		return a.progress, a.progressErr
	}
	var value savedProgress
	err := withStorageLock(a.storageDir, func() error {
		var err error
		value, err = readProgressFile(a.progressFile)
		return err
	})
	if err != nil {
		a.progressErr = err
		return savedProgress{}, err
	}
	a.progress, a.progressErr = value, nil
	return value, nil
}

func (a *App) saveProgress(value savedProgress) error {
	if err := validateProgress(value); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.storageReady {
		if value.Revision != a.progress.Revision {
			return errStaleProgress
		}
		a.progress = value
		return nil
	}
	var stored savedProgress
	err := withStorageLock(a.storageDir, func() error {
		previous, err := readProgressFile(a.progressFile)
		if err != nil {
			return err
		}
		if value.Revision != previous.Revision {
			return errStaleProgress
		}
		stored = value
		stored.Completed = mergeCompleted(previous.Completed, value.Completed)
		if err := writeStoredAtomic(a.progressFile, stored, progressFileMax); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		a.progressErr = err
		return err
	}
	a.progress, a.progressErr = stored, nil
	return nil
}

func (a *App) readUnlockedKeys() ([]unlockedKey, error) {
	if !a.storageReady {
		return keysInOrder(a.unlockedKeys), a.keysStoreErr
	}
	var keys []unlockedKey
	err := withStorageLock(a.storageDir, func() error {
		var err error
		var progress savedProgress
		progress, err = readProgressFile(a.progressFile)
		if err != nil {
			return err
		}
		stored, err := readKeysFileWithRevision(a.keysFile)
		if err != nil {
			return err
		}
		if progress.Revision != a.progress.Revision {
			a.progress = progress
			clear(a.unlockedKeys)
			clear(a.unsavedKeys)
			a.keysEpoch++
			a.cancelKeyVerificationLocked()
		}
		if stored.Revision != progress.Revision {
			keys = []unlockedKey{}
			return nil
		}
		keys = stored.Unlocked
		if len(a.unsavedKeys) == 0 {
			return nil
		}
		byLevel := make(map[int]unlockedKey, len(keys)+len(a.unsavedKeys))
		for _, key := range keys {
			byLevel[key.Level] = key
		}
		for _, key := range a.unsavedKeys {
			byLevel[key.Level] = key
		}
		keys = keysInOrder(byLevel)
		return writeStoredAtomic(a.keysFile, savedKeys{Version: progressVersion, Unlocked: keys, Revision: progress.Revision}, keysFileMax)
	})
	if err != nil {
		a.keysStoreErr = err
		return nil, err
	}
	a.keysStoreErr = nil
	a.unlockedKeys = make(map[int]unlockedKey, len(keys))
	a.unsavedKeys = make(map[int]unlockedKey)
	for _, key := range keys {
		a.unlockedKeys[key.Level] = key
	}
	return keys, nil
}

func (a *App) persistUnlockedKeyLocked(key unlockedKey) error {
	if !a.storageReady {
		return nil
	}
	var keys []unlockedKey
	err := withStorageLock(a.storageDir, func() error {
		progress, err := readProgressFile(a.progressFile)
		if err != nil {
			return err
		}
		if progress.Revision != a.progress.Revision {
			return errStaleProgress
		}
		stored, err := readKeysFileWithRevision(a.keysFile)
		if err != nil {
			return err
		}
		previous := stored.Unlocked
		if stored.Revision != progress.Revision {
			previous = nil
		}
		byLevel := make(map[int]unlockedKey, len(previous)+1)
		for _, saved := range previous {
			byLevel[saved.Level] = saved
		}
		for _, unsaved := range a.unsavedKeys {
			byLevel[unsaved.Level] = unsaved
		}
		byLevel[key.Level] = key
		keys = keysInOrder(byLevel)
		return writeStoredAtomic(a.keysFile, savedKeys{Version: progressVersion, Unlocked: keys, Revision: progress.Revision}, keysFileMax)
	})
	if err != nil {
		a.keysStoreErr = err
		return err
	}
	a.keysStoreErr = nil
	a.unlockedKeys = make(map[int]unlockedKey, len(keys))
	a.unsavedKeys = make(map[int]unlockedKey)
	for _, saved := range keys {
		a.unlockedKeys[saved.Level] = saved
	}
	return nil
}

func (a *App) resetProgress() (savedProgress, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sshConnecting || a.sshSession != nil || a.gitRunning {
		return savedProgress{}, errResetBusy
	}
	reset := defaultProgress()
	if !a.storageReady {
		if a.progress.Revision >= maxProgressRev {
			return savedProgress{}, errors.New("счётчик ревизий переполнен")
		}
		reset.Revision = a.progress.Revision + 1
		a.progress = reset
	} else {
		err := withStorageLock(a.storageDir, func() error {
			previous, err := readProgressFile(a.progressFile)
			if err != nil {
				return err
			}
			if previous.Revision >= maxProgressRev {
				return errors.New("счётчик ревизий переполнен")
			}
			reset.Revision = previous.Revision + 1
			if err := writeStoredAtomic(a.progressFile, reset, progressFileMax); err != nil {
				return err
			}
			// The progress revision hides any old archive even if this cleanup fails.
			_ = writeStoredAtomic(a.keysFile, savedKeys{Version: progressVersion, Unlocked: []unlockedKey{}, Revision: reset.Revision}, keysFileMax)
			return nil
		})
		if err != nil {
			a.progressErr = err
			return savedProgress{}, err
		}
		a.progress = reset
	}
	a.progressErr = nil
	a.keysStoreErr = nil
	clear(a.unlockedKeys)
	clear(a.unsavedKeys)
	a.keysEpoch++
	a.cancelKeyVerificationLocked()
	return reset, nil
}

func keysInOrder(keys map[int]unlockedKey) []unlockedKey {
	result := make([]unlockedKey, 0, len(keys))
	for _, key := range keys {
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Level < result[j].Level })
	return result
}
