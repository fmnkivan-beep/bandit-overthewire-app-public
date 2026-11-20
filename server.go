package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
)

const maxJSONBody = 8192

func localOnly(allowedHost string, next http.Handler) http.Handler {
	allowedOrigin := "http://" + allowedHost
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
		remoteIP := net.ParseIP(remoteHost)
		if err != nil || remoteIP == nil || !remoteIP.IsLoopback() || r.Host != allowedHost {
			http.Error(w, "локальный API недоступен", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "http" || parsed.Host != allowedHost {
				http.Error(w, "источник запроса отклонён", http.StatusForbidden)
				return
			}
			if origin != allowedOrigin {
				http.Error(w, "источник запроса отклонён", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("ожидается JSON")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("в запросе должен быть один JSON-объект")
		}
		return err
	}
	return nil
}

func newHTTPHandler(app *App, events *eventHub, assets fs.FS, allowedHost string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/progress", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		progress, err := app.readProgress()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать сохранённый прогресс")
			return
		}
		writeJSON(w, http.StatusOK, progress)
	})
	mux.HandleFunc("PUT /api/progress", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+allowedHost {
			writeError(w, http.StatusForbidden, "источник запроса отклонён")
			return
		}
		var progress savedProgress
		if err := decodeProgressJSON(w, r, &progress); err != nil {
			writeRequestError(w, err)
			return
		}
		if err := validateProgress(progress); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := app.saveProgress(progress); err != nil {
			if errors.Is(err, errStaleProgress) {
				writeError(w, http.StatusConflict, "прогресс изменился в другом окне; обновите страницу")
				return
			}
			writeError(w, http.StatusInternalServerError, "не удалось сохранить прогресс; старый файл оставлен без изменений")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/progress/reset", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Origin") != "http://"+allowedHost {
			writeError(w, http.StatusForbidden, "источник запроса отклонён")
			return
		}
		if err := decodeEmptyJSON(w, r); err != nil {
			writeRequestError(w, err)
			return
		}
		progress, err := app.resetProgress()
		if errors.Is(err, errResetBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось сохранить сброс прогресса; предыдущий файл оставлен без изменений")
			return
		}
		writeJSON(w, http.StatusOK, progress)
	})
	mux.HandleFunc("GET /api/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		keys, err := app.keysSnapshot()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать архив ключей")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"unlocked": keys})
	})
	mux.HandleFunc("POST /api/keys/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+allowedHost {
			writeError(w, http.StatusForbidden, "источник запроса отклонён")
			return
		}
		if err := app.resetUnlockedKeys(); err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось сохранить сброс архива ключей; подтверждённые ключи оставлены")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/events", events.serveHTTP)
	mux.HandleFunc("GET /api/ssh/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, app.SSHStatus())
	})
	mux.HandleFunc("POST /api/ssh/connect", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decodeJSON(w, r, &input, maxJSONBody); err != nil {
			writeRequestError(w, err)
			return
		}
		if err := ValidateBanditUsername(input.Username); err != nil || input.Password == "" || len(input.Password) > 256 {
			writeError(w, http.StatusBadRequest, "введите корректные данные подключения Bandit")
			return
		}
		if app.SSHStatus()["state"] != "disconnected" {
			writeError(w, http.StatusConflict, "сначала отключитесь от текущего сеанса")
			return
		}
		go func(username, password string) { _ = app.ConnectSSH(username, password) }(input.Username, input.Password)
		writeJSON(w, http.StatusAccepted, map[string]string{"state": "connecting"})
	})
	mux.HandleFunc("POST /api/ssh/input", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Data string `json:"data"`
		}
		if err := decodeJSON(w, r, &input, 6144); err != nil {
			writeRequestError(w, err)
			return
		}
		if input.Data == "" || len(input.Data) > 4096 {
			writeError(w, http.StatusBadRequest, "неверный размер ввода терминала")
			return
		}
		if err := app.SendSSHData(input.Data); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/ssh/resize", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Cols int `json:"cols"`
			Rows int `json:"rows"`
		}
		if err := decodeJSON(w, r, &input, 256); err != nil {
			writeRequestError(w, err)
			return
		}
		if err := app.ResizeSSH(input.Cols, input.Rows); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/ssh/disconnect", func(w http.ResponseWriter, _ *http.Request) {
		if err := app.DisconnectSSH(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/ssh/host-key", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Accept bool `json:"accept"`
		}
		if err := decodeJSON(w, r, &input, 128); err != nil {
			writeRequestError(w, err)
			return
		}
		app.ConfirmHostKey(input.Accept)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/git/connect", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			LevelID  int    `json:"levelId"`
			Password string `json:"password"`
		}
		if err := decodeJSON(w, r, &input, maxJSONBody); err != nil {
			writeRequestError(w, err)
			return
		}
		if err := app.SetGitPassword(input.LevelID, input.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		app.emit("git:status", map[string]string{"state": "connected", "level": fmt.Sprint(input.LevelID)})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/git/command", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			LevelID int    `json:"levelId"`
			Command string `json:"command"`
		}
		if err := decodeJSON(w, r, &input, maxJSONBody); err != nil {
			writeRequestError(w, err)
			return
		}
		if err := app.RunGitCommand(input.LevelID, input.Command); err != nil {
			app.emit("git:status", map[string]string{"state": "error", "message": err.Error()})
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/git/input", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Data string `json:"data"`
		}
		if err := decodeJSON(w, r, &input, 6144); err != nil {
			writeRequestError(w, err)
			return
		}
		if err := app.SendGitData(input.Data); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/git/disconnect", func(w http.ResponseWriter, _ *http.Request) {
		app.DisconnectGit()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /internal/askpass", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 {
			writeError(w, http.StatusBadRequest, "неверный запрос пароля")
			return
		}
		password, ok := app.consumeGitAskpass(r.Header.Get(gitAskpassHeader))
		if !ok {
			writeError(w, http.StatusForbidden, "одноразовый запрос пароля недействителен")
			return
		}
		defer clear(password)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(password)
	})
	if assets != nil {
		if bundle, err := fs.Sub(assets, "frontend/dist"); err == nil {
			mux.Handle("/", http.FileServer(http.FS(bundle)))
		} else {
			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "frontend bundle is missing", http.StatusInternalServerError)
			})
		}
	}
	return localOnly(allowedHost, mux)
}

func decodeEmptyJSON(w http.ResponseWriter, r *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("ожидается JSON")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := rejectDuplicateJSONKeys(body); err != nil || !bytes.Equal(bytes.TrimSpace(body), []byte("{}")) {
		return errors.New("ожидается пустой JSON-объект")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRequestError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "запрос слишком большой")
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
