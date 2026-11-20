import React, { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import './TerminalPanel.css';

async function request(path, body, signal) {
  const response = await fetch(path, {
    method: body === undefined ? 'GET' : 'POST',
    cache: 'no-store',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal
  });
  if (!response.ok) {
    const result = await response.json().catch(() => ({}));
    throw new Error(result.error || `HTTP ${response.status}`);
  }
  return response.status === 204 ? null : response.json();
}

function requestWithTimeout(path, body, timeoutMs = 750) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  return request(path, body, controller.signal).finally(() => clearTimeout(timeout));
}

export function isUnlockedKey(key) {
  return key !== null && typeof key === 'object' && Number.isInteger(key.level) && key.level >= 0 && key.level <= 33 &&
    key.username === `bandit${key.level}` && typeof key.password === 'string' && key.password.length > 0 && new TextEncoder().encode(key.password).byteLength <= 256 &&
    !/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/.test(key.password);
}

export default forwardRef(function TerminalPanel({ mode, onModeChange, onConnectionStatus, levelNum, gitLevelId, gitAvailable, onKeyUnlocked, onKeysSnapshot, onKeySyncStatus }, ref) {
  const hostRef = useRef(null);
  const terminalRef = useRef(null);
  const fitRef = useRef(null);
  const modeRef = useRef(mode);
  const previousModeRef = useRef(mode);
  const previousGitLevelRef = useRef(gitLevelId);
  const previousLevelNumRef = useRef(levelNum);
  const levelNumRef = useRef(levelNum);
  const levelRef = useRef(gitLevelId);
  const sshStateRef = useRef('disconnected');
  const sshSwitchingRef = useRef(false);
  const sshRecoveryPendingRef = useRef(false);
  const sshStatusEventRevisionRef = useRef(0);
  const sshStatusRefreshIdRef = useRef(0);
  const sshStatusRefreshControllerRef = useRef(null);
  const sshStatusVerifiedRef = useRef(false);
  const sshStatusCheckFailedRef = useRef(false);
  const gitConnectGenerationRef = useRef(0);
  const gitConnectInvalidatedRef = useRef(false);
  const gitConnectPendingRef = useRef(false);
  const loginUsernameRef = useRef(`bandit${levelNum}`);
  const loginPasswordRef = useRef('');
  const loginPhaseRef = useRef('username');
  const loginBusyRef = useRef(false);
  const submitSSHPasswordRef = useRef(null);
  const pendingNextLoginRef = useRef(null);
  const automaticSSHAttemptRef = useRef(false);
  const eventsReadyRef = useRef(false);
  const eventsEverReadyRef = useRef(false);
  const switchTokenRef = useRef(0);
  const promptSSHLoginRef = useRef(null);
  const gitLineRef = useRef('');
  const gitHistoryRef = useRef([]);
  const pendingSSHRef = useRef('');
  const sshSendQueueRef = useRef(Promise.resolve());
  const gitCommandQueueRef = useRef(Promise.resolve());
  const sendTimerRef = useRef(null);
  const onKeyUnlockedRef = useRef(onKeyUnlocked);
  const onKeysSnapshotRef = useRef(onKeysSnapshot);
  const onKeySyncStatusRef = useRef(onKeySyncStatus);
  const syncKeysRef = useRef(null);
  const keysResetPendingRef = useRef(false);
  const keysSnapshotVersionRef = useRef(0);
  const [backendReady, setBackendReady] = useState(false);
  const [eventsReady, setEventsReady] = useState(false);
  const [sshState, setSSHState] = useState('checking');
  const [sshStatusVerified, setSSHStatusVerified] = useState(false);
  const [sshStatusCheckFailed, setSSHStatusCheckFailed] = useState(false);
  const [gitConnectCancelling, setGitConnectCancelling] = useState(false);
  const [sshUser, setSSHUser] = useState('');
  const [sshSwitching, setSSHSwitching] = useState(false);
  const [sshRecoveryPending, setSSHRecoveryPending] = useState(false);
  const [gitState, setGitState] = useState('disconnected');
  const [gitPassword, setGitPassword] = useState('');
  const [hostKey, setHostKey] = useState(null);
  const [error, setError] = useState('');

  modeRef.current = mode;
  levelNumRef.current = levelNum;
  levelRef.current = gitLevelId;
  sshStateRef.current = sshState;
  sshSwitchingRef.current = sshSwitching;
  eventsReadyRef.current = eventsReady;
  onKeyUnlockedRef.current = onKeyUnlocked;
  onKeysSnapshotRef.current = onKeysSnapshot;
  onKeySyncStatusRef.current = onKeySyncStatus;

  promptSSHLoginRef.current = () => {
    loginUsernameRef.current = `bandit${levelNumRef.current}`;
    loginPasswordRef.current = '';
    loginPhaseRef.current = 'username';
    loginBusyRef.current = false;
    terminalRef.current?.write(`\r\nlogin as: ${loginUsernameRef.current}`);
  };

  const disconnectSSHAndWait = async () => {
    const deadline = Date.now() + 5000;
    await requestWithTimeout('/api/ssh/disconnect', {});
    while (Date.now() < deadline) {
      const status = await requestWithTimeout('/api/ssh/status', undefined, Math.min(750, deadline - Date.now()));
      if (status.state === 'disconnected') return;
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    throw new Error('SSH-сеанс не отключился. Повторите отключение.');
  };

  const cancelNextLevelLogin = () => {
    const pending = pendingNextLoginRef.current;
    if (!pending && !automaticSSHAttemptRef.current) return;
    if (pending) pending.password = '';
    pendingNextLoginRef.current = null;
    automaticSSHAttemptRef.current = false;
    switchTokenRef.current += 1;
    if (sshSwitchingRef.current) {
      sshSwitchingRef.current = false;
      sshStateRef.current = 'disconnected';
      setSSHSwitching(false);
      setSSHState('disconnected');
    }
  };

  const recoverSSHSwitchFailure = async (error, token, pendingLogin) => {
    if (token !== switchTokenRef.current) return;
    if (pendingLogin) {
      pendingLogin.password = '';
      if (pendingNextLoginRef.current === pendingLogin) pendingNextLoginRef.current = null;
    }
    automaticSSHAttemptRef.current = false;
    loginBusyRef.current = false;
    loginPasswordRef.current = '';
    let status;
    try {
      status = await requestWithTimeout('/api/ssh/status');
    } catch {
      status = null;
    }
    if (token !== switchTokenRef.current) return;
    sshSwitchingRef.current = false;
    setSSHSwitching(false);
    if (status?.state === 'connected') {
      sshRecoveryPendingRef.current = false;
      setSSHRecoveryPending(false);
      sshStateRef.current = 'connected';
      setSSHState('connected');
      setSSHUser(status.user || '');
      loginPhaseRef.current = 'shell';
      setError(`Не удалось отключить прежний SSH-сеанс; активен ${status.user || 'SSH-сеанс'}.`);
      terminalRef.current?.writeln(`[SSH] Не удалось отключить прежний сеанс; активен ${status.user || 'SSH-сеанс'}.`);
    } else if (status?.state === 'disconnected') {
      sshRecoveryPendingRef.current = false;
      setSSHRecoveryPending(false);
      sshStateRef.current = 'disconnected';
      setSSHState('disconnected');
      setSSHUser('');
      setError(error.message);
      terminalRef.current?.writeln(`[SSH] ${error.message}`);
      promptSSHLoginRef.current?.();
    } else if (status?.state === 'connecting') {
      sshRecoveryPendingRef.current = true;
      setSSHRecoveryPending(true);
      sshStateRef.current = 'connecting';
      setSSHState('connecting');
    } else {
      sshRecoveryPendingRef.current = true;
      setSSHRecoveryPending(true);
      sshStateRef.current = 'error';
      setSSHState('error');
      setError('Не удалось проверить состояние SSH после отключения.');
      terminalRef.current?.writeln('[SSH] Не удалось проверить состояние SSH после отключения.');
    }
  };

  useImperativeHandle(ref, () => ({
    focus: () => terminalRef.current?.focus(),
    paste: (text) => terminalRef.current?.paste(text),
    clear: () => terminalRef.current?.clear(),
    notify: (message) => terminalRef.current?.writeln(`\r\n[КЛЮЧИ] ${message}`),
    insertSSHCommand: (command) => {
      if (modeRef.current !== 'ssh' || sshStateRef.current !== 'connected' || loginPhaseRef.current !== 'shell' ||
        !eventsReadyRef.current || sshSwitchingRef.current || sshRecoveryPendingRef.current || !terminalRef.current) return false;
      terminalRef.current.paste(command);
      terminalRef.current.focus();
      return true;
    },
    resetKeys: async (clearKeys) => {
      keysResetPendingRef.current = true;
      keysSnapshotVersionRef.current += 1;
      try {
        await request('/api/keys/reset', {});
        clearKeys();
      } finally {
        keysResetPendingRef.current = false;
        if (await syncKeysRef.current?.() === false) throw new Error('key snapshot unavailable');
      }
    },
    resetProgress: async (onReset) => {
      keysResetPendingRef.current = true;
      keysSnapshotVersionRef.current += 1;
      try {
        const progress = await request('/api/progress/reset', {});
        onReset(progress);
        return progress;
      } finally {
        keysResetPendingRef.current = false;
        if (await syncKeysRef.current?.() === false) throw new Error('key snapshot unavailable');
      }
    },
    prepareNextLevelLogin: (targetLevelNum, vaultPassword) => {
      let password = vaultPassword || '';
      try {
        const terminal = terminalRef.current;
        const selection = terminal?.getSelection() || '';
        if (!password && selection && !/[\r\n\u2028\u2029]/.test(selection) && new TextEncoder().encode(selection).length <= 256) {
          password = selection;
        }
        terminal?.clearSelection();
      } catch {
        password = vaultPassword || '';
      }
      cancelNextLevelLogin();
      pendingNextLoginRef.current = { targetLevelNum, password };
    },
    cancelNextLevelLogin
  }), []);

  useEffect(() => {
    if (!hostRef.current || typeof ResizeObserver === 'undefined') return;
    const terminal = new Terminal({
      allowTransparency: false,
      cursorBlink: true,
      fontFamily: 'JetBrains Mono, Consolas, monospace',
      fontSize: 13,
      lineHeight: 1.45,
      scrollback: 5000,
      disableStdin: true,
      theme: { background: '#000000', foreground: '#f2f2f2', cursor: '#ff5500', selectionBackground: '#62402c' }
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(hostRef.current);
    terminalRef.current = terminal;
    fitRef.current = fit;
    const flushSSH = () => {
      sendTimerRef.current = null;
      const data = pendingSSHRef.current;
      pendingSSHRef.current = '';
      if (!data) return;
      sshSendQueueRef.current = sshSendQueueRef.current.catch(() => {}).then(async () => {
        for (let offset = 0; offset < data.length; offset += 4096) {
          await request('/api/ssh/input', { data: data.slice(offset, offset + 4096) });
        }
      }).catch((err) => terminal.writeln(`\r\n[SSH] ${err.message}`));
    };
    const runGit = (command) => {
      const selectedLevel = levelRef.current;
      gitCommandQueueRef.current = gitCommandQueueRef.current.catch(() => {})
        .then(() => request('/api/git/command', { levelId: selectedLevel, command }))
        .catch((err) => terminal.writeln(`\r\n[GIT] ${err.message}\r\n`));
    };
    const submitPassword = async (autoLogin = false) => {
      const login = loginUsernameRef.current.trim();
      const previousSSHState = sshStateRef.current;
      let secret = loginPasswordRef.current;
      loginPasswordRef.current = '';
      terminal.write('\r\n');
      if (!secret) {
        terminal.writeln('[SSH] Введите пароль Bandit.');
        loginPhaseRef.current = 'password';
        terminal.write('Password: ');
        return;
      }
      loginBusyRef.current = true;
      setError('');
      sshStateRef.current = 'connecting';
      setSSHState('connecting');
      terminal.writeln(`Подключение к ${login}…`);
      try {
        const connection = request('/api/ssh/connect', { username: login, password: secret });
        secret = '';
        await connection;
      } catch (err) {
        loginBusyRef.current = false;
        automaticSSHAttemptRef.current = false;
        const nextSSHState = previousSSHState === 'error' ? 'error' : 'disconnected';
        sshStateRef.current = nextSSHState;
        setSSHState(nextSSHState);
        const message = autoLogin ? 'Автоматический вход не выполнен. Введите пароль вручную.' : err.message;
        setError(message);
        terminal.writeln(`[SSH] ${message}`);
        promptSSHLoginRef.current?.();
      }
    };
    submitSSHPasswordRef.current = submitPassword;
    terminal.onData((data) => {
      if (modeRef.current === 'git') {
        if (data === '\x1b[A') {
          const last = gitHistoryRef.current.at(-1);
          if (last) {
            gitLineRef.current = last;
            terminal.write(`\r\x1b[2K${last}`);
          }
        } else if (!data.startsWith('\x1b')) {
          for (const char of data) {
            if (char === '\r') {
              const command = gitLineRef.current.trim();
              gitLineRef.current = '';
              terminal.write('\r\n');
              terminal.write('git> ');
              if (command) {
                gitHistoryRef.current.push(command);
                runGit(command);
              }
            } else if (char === '\x7f') {
              if (gitLineRef.current) {
                gitLineRef.current = Array.from(gitLineRef.current).slice(0, -1).join('');
                terminal.write('\b \b');
              }
            } else if (char === '\x03') {
              gitLineRef.current = '';
              terminal.write('^C\r\n');
            } else if (char !== '\n' && char !== '\x00' && char >= ' ') {
              gitLineRef.current += char;
              terminal.write(char);
            }
          }
        }
        return;
      }
      if (sshStateRef.current === 'connected') {
        pendingSSHRef.current += data;
        if (sendTimerRef.current === null) sendTimerRef.current = setTimeout(flushSSH, 8);
        return;
      }
      if (sshSwitchingRef.current || loginBusyRef.current || !eventsReadyRef.current) return;
      if (data.startsWith('\x1b')) return;

      for (const char of data) {
        if (char === '\r' || char === '\n') {
          if (char === '\n') continue;
          if (loginPhaseRef.current === 'username') {
            const login = loginUsernameRef.current.trim();
            terminal.write('\r\n');
            if (!login) {
              terminal.writeln('[SSH] Укажите логин Bandit.');
              promptSSHLoginRef.current?.();
              continue;
            }
            loginUsernameRef.current = login;
            loginPhaseRef.current = 'password';
            terminal.write('Password: ');
          } else {
            submitPassword();
            if (loginBusyRef.current) break;
          }
        } else if (char === '\x7f' || char === '\b') {
          if (loginPhaseRef.current === 'username' && loginUsernameRef.current) {
            loginUsernameRef.current = Array.from(loginUsernameRef.current).slice(0, -1).join('');
            terminal.write('\b \b');
          } else if (loginPhaseRef.current === 'password' && loginPasswordRef.current) {
            loginPasswordRef.current = Array.from(loginPasswordRef.current).slice(0, -1).join('');
          }
        } else if (char === '\x03') {
          loginPasswordRef.current = '';
          terminal.write('^C\r\n');
          promptSSHLoginRef.current?.();
        } else if (char >= ' ' && char !== '\x7f') {
          if (loginPhaseRef.current === 'username') {
            loginUsernameRef.current += char;
            terminal.write(char);
          } else if (loginPhaseRef.current === 'password') {
            loginPasswordRef.current += char;
          }
        }
      }
    });

    const fitTerminal = () => {
      if (!hostRef.current?.clientWidth || !hostRef.current?.clientHeight) return;
      try {
        fit.fit();
        if (sshStateRef.current === 'connected') {
          request('/api/ssh/resize', { cols: terminal.cols, rows: terminal.rows }).catch(() => {});
        }
      } catch {
        // The container can briefly report an incomplete size while a panel changes visibility.
      }
    };
    const observer = new ResizeObserver(fitTerminal);
    observer.observe(hostRef.current);
    window.addEventListener('resize', fitTerminal);
    requestAnimationFrame(fitTerminal);

    return () => {
      if (sendTimerRef.current !== null) clearTimeout(sendTimerRef.current);
      if (pendingNextLoginRef.current) pendingNextLoginRef.current.password = '';
      pendingNextLoginRef.current = null;
      const cancelAutomaticAttempt = automaticSSHAttemptRef.current === 'pending' || automaticSSHAttemptRef.current === 'connecting';
      automaticSSHAttemptRef.current = false;
      if (cancelAutomaticAttempt) request('/api/ssh/disconnect', {}).catch(() => {});
      submitSSHPasswordRef.current = null;
      switchTokenRef.current += 1;
      observer.disconnect();
      window.removeEventListener('resize', fitTerminal);
      terminal.dispose();
      terminalRef.current = null;
      fitRef.current = null;
    };
  }, []);

  useEffect(() => {
    if (terminalRef.current) {
      terminalRef.current.options.disableStdin = !eventsReady || (mode === 'ssh' && (sshState === 'connecting' || sshSwitching || sshRecoveryPending));
    }
  }, [eventsReady, mode, sshState, sshSwitching, sshRecoveryPending]);

  useEffect(() => {
    if (previousGitLevelRef.current !== gitLevelId && backendReady) {
      gitConnectGenerationRef.current += 1;
      gitConnectInvalidatedRef.current = true;
      if (gitConnectPendingRef.current) setGitConnectCancelling(true);
      request('/api/git/disconnect', {}).catch(() => {});
      setGitState('disconnected');
      setGitPassword('');
    }
    previousGitLevelRef.current = gitLevelId;
  }, [gitLevelId, backendReady]);

  useEffect(() => {
    if (previousModeRef.current === 'git' && mode === 'ssh' && backendReady) {
      gitConnectGenerationRef.current += 1;
      gitConnectInvalidatedRef.current = true;
      if (gitConnectPendingRef.current) setGitConnectCancelling(true);
      request('/api/git/disconnect', {}).catch(() => {});
      setGitState('disconnected');
      setGitPassword('');
    }
    const nextLevelLogin = pendingNextLoginRef.current;
    const continuingNextLogin = mode === 'ssh' && nextLevelLogin?.targetLevelNum === levelNum;
    if (previousModeRef.current !== mode && !continuingNextLogin) cancelNextLevelLogin();
    previousModeRef.current = mode;
  }, [mode, backendReady]);

  useEffect(() => {
    const terminal = terminalRef.current;
    if (!terminal) return;
    terminal.write('\x1b[2J\x1b[H');
    gitLineRef.current = '';
    const isNewLevel = previousLevelNumRef.current !== levelNum;
    previousLevelNumRef.current = levelNum;
    const pendingLogin = pendingNextLoginRef.current;
    if (pendingLogin && pendingLogin.targetLevelNum !== levelNum) cancelNextLevelLogin();
    const nextLogin = pendingLogin?.targetLevelNum === levelNum ? pendingLogin : null;
    if (mode === 'ssh') {
      terminal.writeln('SSH // bandit.labs.overthewire.org:2220');
      if ((isNewLevel || nextLogin) && backendReady) {
        const token = ++switchTokenRef.current;
        sshSwitchingRef.current = true;
        setSSHSwitching(true);
        sshStateRef.current = 'connecting';
        setSSHState('connecting');
        terminal.writeln('Уровень изменён. Отключаю предыдущий сеанс…');
        disconnectSSHAndWait().then(() => {
          if (token !== switchTokenRef.current || (nextLogin && pendingNextLoginRef.current !== nextLogin)) return;
          setSSHUser('');
          sshStateRef.current = 'disconnected';
          setSSHState('disconnected');
          sshSwitchingRef.current = false;
          setSSHSwitching(false);
          if (nextLogin) {
            const password = nextLogin.password;
            nextLogin.password = '';
            pendingNextLoginRef.current = null;
            if (password && eventsReadyRef.current && submitSSHPasswordRef.current && modeRef.current === 'ssh') {
              loginUsernameRef.current = `bandit${levelNumRef.current}`;
              loginPhaseRef.current = 'password';
              loginPasswordRef.current = password;
              automaticSSHAttemptRef.current = 'pending';
              submitSSHPasswordRef.current?.(true);
            } else {
              terminal.writeln('Введите пароль для нового уровня.');
              promptSSHLoginRef.current?.();
            }
          } else {
            terminal.writeln('Введите пароль для нового уровня.');
            promptSSHLoginRef.current?.();
          }
          terminal.focus();
        }).catch((err) => recoverSSHSwitchFailure(err, token, nextLogin));
      } else if (backendReady && sshStateRef.current !== 'connected' && !sshSwitchingRef.current) {
        promptSSHLoginRef.current?.();
      }
    } else {
      terminal.writeln(`GIT // локальная рабочая копия · этап ${gitLevelId}`);
      terminal.write('git> ');
    }
    fitRef.current?.fit();
  }, [mode, gitLevelId, levelNum, backendReady]);

  useEffect(() => {
    let source;
    let active = true;
    let keySyncFailed = false;
    const keySyncError = 'Не удалось обновить архив ключей. Повторите действие или откройте приложение заново.';
    const syncKeys = async () => {
      if (!active || keysResetPendingRef.current) return;
      const version = ++keysSnapshotVersionRef.current;
      try {
        const snapshot = await request('/api/keys');
        if (!active || keysResetPendingRef.current || version !== keysSnapshotVersionRef.current) return;
        if (!Array.isArray(snapshot?.unlocked)) throw new Error('invalid key snapshot');
        const unlocked = snapshot.unlocked.filter(isUnlockedKey);
        if (onKeysSnapshotRef.current) onKeysSnapshotRef.current(unlocked);
        else unlocked.forEach((key) => onKeyUnlockedRef.current?.(key));
        if (keySyncFailed) {
          keySyncFailed = false;
          setError((previous) => previous === keySyncError ? '' : previous);
          onKeySyncStatusRef.current?.('Архив ключей обновлён.');
        }
        return true;
      } catch {
        if (!active || keysResetPendingRef.current || version !== keysSnapshotVersionRef.current) return;
        keySyncFailed = true;
        setError(keySyncError);
        onKeySyncStatusRef.current?.(keySyncError);
        return false;
      }
    };
    syncKeysRef.current = syncKeys;
    request('/api/ssh/status').then((status) => {
      if (!active) return;
      setBackendReady(true);
      sshStateRef.current = status.state;
      setSSHState(status.state);
      sshStatusVerifiedRef.current = true;
      setSSHStatusVerified(true);
      sshStatusCheckFailedRef.current = false;
      setSSHStatusCheckFailed(false);
      if (status.state === 'connected') loginPhaseRef.current = 'shell';
      if (status.user) setSSHUser(status.user);
      source = new EventSource('/api/events');
      const listen = (name, handler) => source.addEventListener(name, (event) => {
        if (active) handler(event);
      });
      source.onopen = () => {
        if (!active) return;
        const isReconnect = eventsEverReadyRef.current;
        eventsEverReadyRef.current = true;
        setEventsReady(true);
        syncKeys();
        if (isReconnect) {
          sshStatusRefreshControllerRef.current?.abort();
          const controller = new AbortController();
          sshStatusRefreshControllerRef.current = controller;
          const refreshId = ++sshStatusRefreshIdRef.current;
          const eventRevision = sshStatusEventRevisionRef.current;
          const timeout = setTimeout(() => controller.abort(), 750);
          sshStatusVerifiedRef.current = false;
          setSSHStatusVerified(false);
          sshStatusCheckFailedRef.current = false;
          setSSHStatusCheckFailed(false);
          request('/api/ssh/status', undefined, controller.signal).then((freshStatus) => {
            if (!active || refreshId !== sshStatusRefreshIdRef.current) return;
            if (eventRevision === sshStatusEventRevisionRef.current) {
              sshStateRef.current = freshStatus.state;
              setSSHState(freshStatus.state);
              if (freshStatus.state === 'connected') loginPhaseRef.current = 'shell';
              if (freshStatus.user) setSSHUser(freshStatus.user);
              if (freshStatus.state === 'disconnected') setSSHUser('');
            }
            sshStatusVerifiedRef.current = true;
            setSSHStatusVerified(true);
          }).catch(() => {
            if (!active || refreshId !== sshStatusRefreshIdRef.current) return;
            if (eventRevision !== sshStatusEventRevisionRef.current) {
              sshStatusCheckFailedRef.current = false;
              setSSHStatusCheckFailed(false);
              sshStatusVerifiedRef.current = true;
              setSSHStatusVerified(true);
              return;
            }
            sshStatusCheckFailedRef.current = true;
            setSSHStatusCheckFailed(true);
          }).finally(() => {
            clearTimeout(timeout);
            if (sshStatusRefreshControllerRef.current === controller) sshStatusRefreshControllerRef.current = null;
          });
        }
      };
      source.onerror = () => {
        if (!active) return;
        setEventsReady(false);
        sshStatusRefreshIdRef.current += 1;
        sshStatusRefreshControllerRef.current?.abort();
        sshStatusRefreshControllerRef.current = null;
      };
      listen('keys:unlocked', (event) => {
        try {
          const key = JSON.parse(event.data);
          if (isUnlockedKey(key)) syncKeys();
        } catch { /* Ignore malformed events without exposing their contents. */ }
      });
      listen('keys:storage-error', (event) => {
        const value = JSON.parse(event.data);
        const message = value.message || 'Не удалось сохранить архив ключей.';
        setError(message);
        onKeySyncStatusRef.current?.(message);
      });
      listen('ssh:data', (event) => terminalRef.current?.write(JSON.parse(event.data)));
      listen('ssh:status', (event) => {
        const value = JSON.parse(event.data);
        sshStatusEventRevisionRef.current += 1;
        sshStateRef.current = value.state;
        setSSHState(value.state);
        if (value.state === 'connecting') setError('');
        if (value.user) setSSHUser(value.user);
        if (value.state === 'disconnected') setSSHUser('');
        if (value.state === 'connecting' && automaticSSHAttemptRef.current === 'pending') automaticSSHAttemptRef.current = 'connecting';
        const automaticFailure = (automaticSSHAttemptRef.current === 'pending' && value.state === 'error') ||
          (automaticSSHAttemptRef.current === 'connecting' && (value.state === 'error' || value.state === 'disconnected'));
        const waitingForAutomaticConnect = automaticSSHAttemptRef.current === 'pending' && (value.state === 'error' || value.state === 'disconnected');
        if (value.state === 'connected') automaticSSHAttemptRef.current = false;
        if (sshRecoveryPendingRef.current && (value.state === 'connected' || value.state === 'disconnected')) {
          sshRecoveryPendingRef.current = false;
          setSSHRecoveryPending(false);
        }
        if (automaticFailure) {
          automaticSSHAttemptRef.current = false;
          setError('Автоматический вход не выполнен. Введите пароль вручную.');
          terminalRef.current?.writeln('\r\n[SSH] Автоматический вход не выполнен.');
        } else if (value.message && !waitingForAutomaticConnect) {
          setError(value.message);
          terminalRef.current?.writeln(`\r\n[SSH] ${value.message}`);
        }
        if (value.state === 'error' || value.state === 'disconnected') setHostKey(null);
        if ((value.state === 'error' || value.state === 'disconnected') && modeRef.current === 'ssh' && !sshSwitchingRef.current && !automaticSSHAttemptRef.current && (!sshRecoveryPendingRef.current || value.state === 'disconnected')) {
          loginBusyRef.current = false;
          terminalRef.current?.writeln('');
          promptSSHLoginRef.current?.();
        }
        if (value.state === 'connected') {
          loginBusyRef.current = false;
          loginPhaseRef.current = 'shell';
          request('/api/ssh/resize', { cols: terminalRef.current?.cols || 100, rows: terminalRef.current?.rows || 30 }).catch(() => {});
        }
      });
      if (status.state === 'connected' && status.user !== `bandit${levelNumRef.current}`) {
        const token = ++switchTokenRef.current;
        sshSwitchingRef.current = true;
        setSSHSwitching(true);
        sshStateRef.current = 'connecting';
        setSSHState('connecting');
        terminalRef.current?.writeln('Отключаю сеанс другого уровня…');
        disconnectSSHAndWait().then(() => {
          if (token !== switchTokenRef.current) return;
          setSSHUser('');
          sshStateRef.current = 'disconnected';
          setSSHState('disconnected');
          sshSwitchingRef.current = false;
          setSSHSwitching(false);
          promptSSHLoginRef.current?.();
        }).catch((err) => recoverSSHSwitchFailure(err, token));
      }
      listen('ssh:host-key', (event) => setHostKey(JSON.parse(event.data)));
      listen('git:output', (event) => terminalRef.current?.write(JSON.parse(event.data)));
      listen('git:status', (event) => {
        const value = JSON.parse(event.data);
        // The POST response sets connected; a delayed SSE connected event may be stale after disconnect.
        if (value.state === 'connected') return;
        if (modeRef.current !== 'git' && value.state !== 'disconnected') return;
        if (gitConnectInvalidatedRef.current && value.state !== 'disconnected') return;
        setGitState(value.state);
        if (value.message) terminalRef.current?.writeln(`\r\n[GIT] ${value.message}\r\n`);
      });
    }).catch(() => {
      if (active) {
        setBackendReady(false);
        setSSHState('unavailable');
      }
    });
    return () => {
      active = false;
      source?.close();
      sshStatusRefreshIdRef.current += 1;
      sshStatusRefreshControllerRef.current?.abort();
      sshStatusRefreshControllerRef.current = null;
      syncKeysRef.current = null;
    };
  }, []);

  const saveGitPassword = async (event) => {
    event.preventDefault();
    if (gitConnectPendingRef.current || gitConnectCancelling) return;
    setError('');
    const submittedPassword = gitPassword;
    const connectGeneration = ++gitConnectGenerationRef.current;
    gitConnectInvalidatedRef.current = false;
    gitConnectPendingRef.current = true;
    setGitConnectCancelling(false);
    setGitPassword('');
    setGitState('connecting');
    let staleConnectCleanupSucceeded = true;
    try {
      await request('/api/git/connect', { levelId: gitLevelId, password: submittedPassword });
      if (connectGeneration !== gitConnectGenerationRef.current || modeRef.current !== 'git') {
        try {
          await request('/api/git/disconnect', {});
        } catch (err) {
          staleConnectCleanupSucceeded = false;
          setGitState('error');
          setError(`Не удалось завершить отключение Git: ${err.message}`);
        }
        return;
      }
      setGitState('connected');
      terminalRef.current?.writeln('[GIT] Пароль для этого этапа сохранён в памяти до закрытия приложения.');
      terminalRef.current?.write('git> ');
      terminalRef.current?.focus();
    } catch (err) {
      if (connectGeneration !== gitConnectGenerationRef.current || modeRef.current !== 'git') return;
      setGitState('error');
      setError(err.message);
    } finally {
      gitConnectPendingRef.current = false;
      if (staleConnectCleanupSucceeded) setGitConnectCancelling(false);
      if (connectGeneration === gitConnectGenerationRef.current) setGitPassword('');
    }
  };

  const disconnect = async () => {
    cancelNextLevelLogin();
    automaticSSHAttemptRef.current = false;
    setError('');
    if (mode === 'ssh') {
      const token = ++switchTokenRef.current;
      sshSwitchingRef.current = true;
      setSSHSwitching(true);
      sshStateRef.current = 'connecting';
      setSSHState('connecting');
      try {
        await disconnectSSHAndWait();
        if (token !== switchTokenRef.current) return;
        setSSHUser('');
        sshRecoveryPendingRef.current = false;
        setSSHRecoveryPending(false);
        sshStateRef.current = 'disconnected';
        setSSHState('disconnected');
        sshSwitchingRef.current = false;
        setSSHSwitching(false);
        terminalRef.current?.writeln('\r\n[SSH] Сеанс отключён.');
        promptSSHLoginRef.current?.();
      } catch (err) {
        await recoverSSHSwitchFailure(err, token);
      }
      return;
    }
    gitConnectGenerationRef.current += 1;
    gitConnectInvalidatedRef.current = true;
    if (gitConnectPendingRef.current) setGitConnectCancelling(true);
    setGitState('disconnected');
    setGitPassword('');
    try {
      await request('/api/git/disconnect', {});
      if (!gitConnectPendingRef.current) setGitConnectCancelling(false);
    } catch (err) {
      setGitState('error');
      setError(err.message);
    }
  };

  const decideHostKey = async (accept) => {
    try {
      await request('/api/ssh/host-key', { accept });
      setHostKey(null);
    } catch (err) {
      setError(err.message);
    }
  };

  const currentState = mode === 'ssh' ? sshState : gitState;
  const state = mode === 'ssh'
    ? currentState === 'checking' ? 'checking'
      : !backendReady ? 'unavailable'
        : !eventsReady ? (eventsEverReadyRef.current ? 'disconnected' : 'checking')
          : !sshStatusVerified ? sshStatusCheckFailed
            ? (['disconnected', 'error', 'unavailable'].includes(currentState) ? currentState : 'error')
            : 'checking'
            : sshSwitching || sshRecoveryPending ? 'connecting' : currentState
    : currentState === 'connecting' ? 'connecting'
      : !backendReady ? 'unavailable'
        : !eventsReady ? (eventsEverReadyRef.current ? 'disconnected' : 'checking')
          : currentState;
  const eventStreamLost = !eventsReady && eventsEverReadyRef.current;
  const activeSSH = sshState === 'connected' || sshState === 'connecting';
  const stateLabel = eventStreamLost ? 'НЕТ СВЯЗИ' : ({
    checking: 'ПРОВЕРКА',
    unavailable: 'НЕТ СВЯЗИ',
    disconnected: 'НЕ ПОДКЛЮЧЕНО',
    connecting: 'ПОДКЛЮЧЕНИЕ',
    connected: mode === 'ssh' ? `SSH · ${sshUser || `bandit${levelNum}`}` : 'GIT · ГОТОВ',
    running: 'GIT · ВЫПОЛНЯЕТСЯ',
    finished: 'GIT · ГОТОВ',
    error: 'ОШИБКА'
  }[state] || state.toUpperCase());

  const connectedState = ['connected', 'running', 'finished'].includes(state);
  const connectingState = ['checking', 'connecting'].includes(state);
  const connectionStatus = {
    mode,
    state: connectedState ? 'connected' : connectingState ? 'connecting' : state === 'unavailable' ? 'unavailable' : state === 'error' ? 'error' : 'disconnected',
    label: eventStreamLost ? 'НЕТ СВЯЗИ' : state === 'checking' ? 'ПРОВЕРКА' : state === 'connecting' ? 'ПОДКЛЮЧЕНИЕ'
      : state === 'running' ? 'ВЫПОЛНЯЕТСЯ' : connectedState ? 'ПОДКЛЮЧЕНО'
        : state === 'unavailable' ? 'НЕДОСТУПНО' : state === 'error' ? 'ОШИБКА' : 'ОТКЛЮЧЕНО'
  };

  useEffect(() => {
    onConnectionStatus?.(connectionStatus);
  }, [mode, connectionStatus.state, connectionStatus.label, onConnectionStatus]);

  return (
    <div className="terminal-panel">
      <div className="terminal-connection-bar">
        <div className="terminal-mode-buttons" role="group" aria-label="Режим терминала">
          <button type="button" className={mode === 'ssh' ? 'is-active' : ''} disabled={!eventsReady || gitState === 'running'} onClick={() => onModeChange('ssh')}>SSH</button>
          <button type="button" className={mode === 'git' ? 'is-active' : ''} disabled={!eventsReady || !gitAvailable || activeSSH} onClick={() => onModeChange('git')}>GIT</button>
        </div>
        <span className={`terminal-state terminal-state-${state}`} role="status">{stateLabel}</span>
        {backendReady && ((mode === 'ssh' && (activeSSH || sshState === 'error')) || (mode === 'git' && gitState !== 'disconnected' && gitState !== 'unavailable')) && (
          <button type="button" className="btn-wire terminal-disconnect" onClick={disconnect}>ОТКЛЮЧИТЬ</button>
        )}
      </div>

      {!backendReady ? (
        <div className="terminal-backend-notice" role="status">
          <strong>Нужен BANDIT.exe</strong>
          <span>Предпросмотр в браузере не запускает оболочку. Откройте приложение для подключения к Bandit.</span>
        </div>
      ) : mode === 'git' ? (
        <form className="terminal-auth-form terminal-git-auth" onSubmit={saveGitPassword}>
          <label>ПАРОЛЬ GIT ДЛЯ ЭТАПА {gitLevelId}
            <input aria-label="Пароль Git" type="password" value={gitPassword} onChange={(event) => setGitPassword(event.target.value)} autoComplete="off" disabled={gitConnectCancelling || gitState === 'connecting'} />
          </label>
          <button type="submit" className="btn-invert-primary" disabled={!gitAvailable || gitConnectCancelling || gitState === 'connecting'}>СОХРАНИТЬ В ПАМЯТИ</button>
        </form>
      ) : null}

      {error && <div className="terminal-error" role="alert">{error}</div>}
      <div ref={hostRef} className="terminal-surface" role="application" aria-label={mode === 'ssh' ? `Терминал SSH для bandit${levelNum}` : 'Ограниченная консоль Git'} />

      {hostKey && (
        <div className="terminal-hostkey-backdrop">
          <section className="terminal-hostkey" role="dialog" aria-modal="true" aria-labelledby="terminal-hostkey-title">
            <h2 id="terminal-hostkey-title">Проверка ключа SSH</h2>
            <p>Подтвердите, что вы доверяете серверу по указанному адресу. При несовпадении уже сохранённого ключа подключение будет отклонено.</p>
            <code>{hostKey.address}<br />{hostKey.keyType} · {hostKey.fingerprint}</code>
            <div>
              <button type="button" className="btn-wire" onClick={() => decideHostKey(false)}>ОТКЛОНИТЬ</button>
              <button type="button" className="btn-invert-primary" onClick={() => decideHostKey(true)}>ДОВЕРЯТЬ ЭТОМУ КЛЮЧУ</button>
            </div>
          </section>
        </div>
      )}
    </div>
  );
});
