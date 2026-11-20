import { test, beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { build } from 'esbuild';
import { JSDOM } from 'jsdom';
import React, { act } from 'react';

const bootstrap = new JSDOM('<div></div>');
globalThis.window = bootstrap.window;
globalThis.document = bootstrap.window.document;
const { createRoot } = await import('react-dom/client');

await mkdir('.test-build', { recursive: true });
await build({
  entryPoints: ['src/App.tsx'], bundle: true, platform: 'node', format: 'cjs', outfile: '.test-build/App.cjs', external: ['react'],
  plugins: [{
    name: 'xterm-test-double',
    setup(build) {
      build.onResolve({ filter: /^@xterm\/(xterm|addon-fit)$/ }, () => ({ path: 'xterm-test-double', namespace: 'xterm-test-double' }));
      build.onLoad({ filter: /.*/, namespace: 'xterm-test-double' }, () => ({
        contents: `
          const state = globalThis.__xtermMock ||= { instances: [] };
          export class Terminal {
            constructor(options) { this.options = options; this.selection = ''; this.output = ''; this.listeners = []; this.cols = 80; this.rows = 24; state.instances.push(this); }
            loadAddon() {} open() {} focus() { this.focused = true; } paste(text) { (this.pasted ||= []).push(text); this.send(text); } clear() { this.output = ''; }
            write(text) { this.output += text; } writeln(text) { this.output += text + '\\n'; }
            getSelection() { return this.selection; }
            clearSelection() { this.selection = ''; }
            onData(listener) { this.listeners.push(listener); return { dispose() {} }; }
            send(data) { this.listeners.forEach((listener) => listener(data)); }
            dispose() {}
          }
          export class FitAddon { fit() {} }
        `,
        loader: 'js'
      }));
    }
  }]
});
const require = createRequire(import.meta.url);
const App = require('../.test-build/App.cjs').default;
let dom, root, media, listener, originalGlobals;
const button = (text) => [...document.querySelectorAll('button')].find((el) => el.textContent.includes(text));
const exactButton = (text) => [...document.querySelectorAll('button')].find((el) => el.textContent.trim() === text);
const summary = (text) => [...document.querySelectorAll('summary')].find((el) => el.textContent.trim() === text);
const click = async (el) => { assert.ok(el, 'control exists'); await act(async () => el.click()); };
const edit = async (el, value) => {
  const setValue = Object.getOwnPropertyDescriptor(el.constructor.prototype, 'value').set;
  await act(async () => { setValue.call(el, value); el.dispatchEvent(new dom.window.Event('input', { bubbles: true })); });
};
const mount = async () => { root = createRoot(document.getElementById('root')); await act(async () => root.render(React.createElement(App))); };

beforeEach(() => {
  originalGlobals = { fetch: globalThis.fetch, EventSource: globalThis.EventSource, ResizeObserver: globalThis.ResizeObserver };
  globalThis.__xtermMock.instances.length = 0;
  dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  Object.assign(globalThis, { window: dom.window, document: dom.window.document, localStorage: dom.window.localStorage, HTMLElement: dom.window.HTMLElement, IS_REACT_ACT_ENVIRONMENT: true });
  globalThis.fetch = async (path) => path === '/api/progress'
    ? { ok: false, status: 404, json: async () => ({}) }
    : Promise.reject(new Error(`Unexpected request ${path}`));
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: dom.window.navigator });
  dom.window.HTMLElement.prototype.scrollIntoView = () => {};
  dom.window.HTMLCanvasElement.prototype.getContext = () => null;
  dom.window.HTMLDialogElement.prototype.showModal = function () { this.open = true; this.querySelector('button')?.focus(); };
  dom.window.HTMLDialogElement.prototype.close = function () { this.open = false; this.dispatchEvent(new dom.window.Event('close')); };
  media = { matches: false, addEventListener: (_, fn) => { listener = fn; }, removeEventListener: () => {} };
  dom.window.matchMedia = () => media;
  globalThis.requestAnimationFrame = () => 1;
  globalThis.cancelAnimationFrame = () => {};
});
afterEach(async () => {
  if (root) await act(async () => root.unmount());
  root = null;
  for (const [key, value] of Object.entries(originalGlobals)) {
    if (value === undefined) delete globalThis[key];
    else globalThis[key] = value;
  }
  dom.window.close();
});

const settle = async () => act(async () => {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
});
const startSSHBackend = async ({ delayDisconnect = false, disconnectError = false, disconnectStateOnError = 'disconnected', hangDisconnect = false, disconnectStateOnAbort = 'disconnected', statusFailureAfter = Infinity, hangStatusAfter = Infinity, statusStateAfter = null, delayStatusRefresh = false, snapshot = [], resetError = false, progressResetError = false, beforeResetResponse, completedLevels, progress, progressPutError = false, gitConnectError = false, delayGitConnect = false } = {}) => {
  if (completedLevels) localStorage.setItem('otw_bandit_completed', JSON.stringify(completedLevels));
  const requests = [];
  let serverKeys = snapshot;
  let serverProgress = progress;
  let state = 'connected';
  let statusCalls = 0;
  let shouldDelayDisconnect = delayDisconnect;
  let shouldHangDisconnect = hangDisconnect;
  let gitPasswordStored = false;
  const disconnectReleases = [];
  let shouldDelayStatusRefresh = delayStatusRefresh;
  const statusRefreshReleases = [];
  let shouldDelayGitConnect = delayGitConnect;
  const gitConnectReleases = [];
  class TestEventSource {
    constructor() {
      this.listeners = new Map();
      const reopening = !!TestEventSource.instance;
      TestEventSource.instance = this;
      if (reopening) queueMicrotask(() => this.open());
    }
    addEventListener(name, fn) { this.listeners.set(name, fn); }
    open() { this.onopen?.(); }
    emit(name, data) {
      if (name === 'keys:unlocked' && data && Number.isInteger(data.level) && data.level >= 0 && data.level <= 33 && data.username === `bandit${data.level}` && typeof data.password === 'string') {
        serverKeys = [...(Array.isArray(serverKeys) ? serverKeys : []).filter((key) => key.level !== data.level), data];
      }
      this.deliver(name, data);
    }
    deliver(name, data) { this.listeners.get(name)?.({ data: JSON.stringify(data) }); }
    close() { this.closed = true; }
  }
  globalThis.EventSource = TestEventSource;
  globalThis.ResizeObserver = class { observe() {} disconnect() {} };
  globalThis.fetch = async (path, options = {}) => {
    const body = options.body ? JSON.parse(options.body) : undefined;
    requests.push({ path, body });
    if (path === '/api/keys') {
      const captured = serverKeys;
      if (captured instanceof Error) return { ok: false, status: 503, json: async () => ({ error: 'snapshot unavailable' }) };
      return { ok: true, status: 200, json: async () => ({ unlocked: await captured }) };
    }
    if (path === '/api/progress') {
      if (options.method === 'PUT') {
        if (progressPutError) return { ok: false, status: 500, json: async () => ({ error: 'progress save failed' }) };
        serverProgress = body;
        return { ok: true, status: 204 };
      }
      return serverProgress === undefined
        ? { ok: false, status: 404, json: async () => ({}) }
        : { ok: true, status: 200, json: async () => serverProgress };
    }
    if (path === '/api/progress/reset') {
      if (!progressResetError) {
        serverKeys = [];
        serverProgress = { version: 1, activeLevelId: '0', completed: { '0': false }, revision: (serverProgress?.revision || 0) + 1 };
      }
      return { ok: !progressResetError, status: progressResetError ? 409 : 200, json: async () => progressResetError ? { error: 'disconnect first' } : serverProgress };
    }
    if (path === '/api/keys/reset') {
      if (!resetError) serverKeys = [];
      await beforeResetResponse?.({ setSnapshot: (keys) => { serverKeys = keys; }, source: TestEventSource.instance });
      return { ok: !resetError, status: resetError ? 403 : 204, json: async () => ({ error: 'reset failed' }) };
    }
    if (path === '/api/ssh/input') return { ok: true, status: 204 };
    if (path === '/api/ssh/status') {
      statusCalls += 1;
      if (statusCalls > hangStatusAfter && state !== 'disconnected') {
        return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('status timeout')), { once: true }));
      }
      if (statusCalls > statusFailureAfter) return { ok: false, status: 503, json: async () => ({ error: 'status unavailable' }) };
      const reportedState = statusCalls > 1 && statusStateAfter ? statusStateAfter : state;
      if (statusCalls > 1 && shouldDelayStatusRefresh) {
        shouldDelayStatusRefresh = false;
        await new Promise((resolve, reject) => {
          statusRefreshReleases.push(resolve);
          options.signal?.addEventListener('abort', () => reject(new Error('status refresh aborted')), { once: true });
        });
      }
      return { ok: true, status: 200, json: async () => ({ state: reportedState, user: reportedState === 'connected' ? 'bandit0' : undefined }) };
    }
    if (path === '/api/ssh/disconnect') {
      if (shouldDelayDisconnect) await new Promise((resolve) => { disconnectReleases.push(resolve); });
      if (shouldHangDisconnect) {
        return new Promise((_, reject) => options.signal.addEventListener('abort', () => { state = disconnectStateOnAbort; reject(new Error('disconnect timeout')); }, { once: true }));
      }
      if (disconnectError) {
        state = disconnectStateOnError;
        return { ok: false, status: 500, json: async () => ({ error: 'disconnect failed' }) };
      }
      state = 'disconnected';
      return { ok: true, status: 204 };
    }
    if (path === '/api/ssh/connect') return { ok: true, status: 202, json: async () => ({ state: 'connecting' }) };
    if (path === '/api/git/connect') {
      if (shouldDelayGitConnect) await new Promise((resolve) => gitConnectReleases.push(resolve));
      if (gitConnectError) return { ok: false, status: 400, json: async () => ({ error: 'git login failed' }) };
      gitPasswordStored = true;
      TestEventSource.instance?.emit('git:status', { state: 'connected' });
      return { ok: true, status: 204 };
    }
    if (path === '/api/ssh/resize') return { ok: true, status: 204 };
    if (path === '/api/git/command') return { ok: true, status: 204 };
    if (path === '/api/git/disconnect') {
      gitPasswordStored = false;
      TestEventSource.instance?.emit('git:status', { state: 'disconnected' });
      return { ok: true, status: 204 };
    }
    throw new Error(`Unexpected request ${path}`);
  };
  await mount();
  await settle();
  await act(async () => TestEventSource.instance.open());
  await settle();
  return {
    requests,
    currentKeys: () => serverKeys,
    currentProgress: () => serverProgress,
    source: TestEventSource.instance,
    get currentSource() { return TestEventSource.instance; },
    setSnapshot: (keys) => { serverKeys = keys; },
    terminal: globalThis.__xtermMock.instances.at(-1),
    delayNextDisconnect: () => { shouldDelayDisconnect = true; },
    allowDisconnectRetry: () => { shouldHangDisconnect = false; },
    releaseDisconnect: () => disconnectReleases.splice(0).forEach((release) => release()),
    delayNextStatusRefresh: () => { shouldDelayStatusRefresh = true; },
    releaseStatusRefresh: () => statusRefreshReleases.splice(0).forEach((release) => release()),
    setBackendSSHState: (nextState) => { state = nextState; },
    hasGitPassword: () => gitPasswordStored,
    releaseGitConnect: () => gitConnectReleases.splice(0).forEach((release) => release())
  };
};
const storageEntries = () => Array.from({ length: localStorage.length }, (_, index) => {
  const key = localStorage.key(index);
  return [key, localStorage.getItem(key)];
});

test('solution accents preserve safe identifiers and leave command steps plain', async () => {
  await startSSHBackend({ progress: { version: 1, activeLevelId: '0-1', completed: {} } });
  await click(button('Решение'));
  const panel = document.querySelector('#mission-solution-panel');
  const paragraph = panel.querySelector('p');
  assert.deepEqual([...paragraph.querySelectorAll('code')].map((code) => code.textContent), ['ls -la', 'readme', 'cat readme', 'bandit1']);
  assert.equal(paragraph.textContent, 'Начни с ls -la. В списке найди readme, затем выполни cat readme и прочитай содержимое в терминале. Используй найденный пароль, чтобы войти по SSH как bandit1 на порт 2220.');
  assert.equal(panel.querySelector(':scope > code').textContent, 'cat readme');
  assert.ok(button('Вставить команду'));

  await click(button('[08]'));
  await click(button('Решение'));
  let laterSolution = document.querySelector('#mission-solution-panel p');
  assert.deepEqual([...laterSolution.querySelectorAll('code')].map((code) => code.textContent), ['bandit8']);
  assert.match(laterSolution.textContent, /`grep millionth data\.txt`/);

  await click(button('[09]'));
  await click(button('Решение'));
  laterSolution = document.querySelector('#mission-solution-panel p');
  assert.deepEqual([...laterSolution.querySelectorAll('code')].map((code) => code.textContent), ['bandit9']);
  assert.match(laterSolution.textContent, /`sort data\.txt \| uniq -u`/);

  await click(button('[29]'));
  await click(button('Решение'));
  laterSolution = document.querySelector('#mission-solution-panel p');
  assert.ok([...laterSolution.querySelectorAll('code')].some((code) => code.textContent === 'README.md'));
  assert.ok([...laterSolution.querySelectorAll('code')].some((code) => code.textContent === 'bandit29'));
  assert.match(laterSolution.textContent, /`git clone`/);
});

test('desktop restores saved progress and writes later level changes through the progress API', async () => {
  const progress = { version: 1, activeLevelId: '1-2', completed: { '0': true, '1-2': true } };
  const { requests } = await startSSHBackend({ progress });
  await settle();
  assert.equal(document.querySelector('[aria-current="step"]')?.textContent.includes('[02]'), true);
  assert.equal(document.querySelector('.app-header-progress')?.getAttribute('aria-label'), 'Пройдено 2 из 34 уровней');
  await click(button('[03]'));
  await settle();
  assert.ok(requests.some(({ path, body }) => path === '/api/progress' && body?.activeLevelId === '2-3'));
});

test('desktop key snapshot hides but preserves old origin localStorage keys', async () => {
  const legacy = JSON.stringify([keyFixture(0)]);
  localStorage.setItem('otw_bandit_unlocked_keys', legacy);
  await startSSHBackend({ progress: { version: 1, activeLevelId: '0', completed: { '0': false } }, snapshot: [] });
  await click(button('Ключи'));
  assert.match(keyRow(0).textContent, /Заблокирован/);
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'), legacy);
});

const moveToLevelOne = async (terminal) => {
  await click(button('[02]'));
  await settle();
  terminal.selection = '';
};
const moveToGitLevel = async (terminal) => {
  await click(button('[28]'));
  await settle();
  await click(document.querySelector('.terminal-mode-buttons button:nth-child(2)'));
  await settle();
  terminal.selection = '';
};

const keyFixture = (level) => ({ level, username: `bandit${level}`, password: `fixture-key-${level}` });
const keyRow = (level) => document.querySelector(`[data-key-level="${level}"]`);
const rowAction = (level, label) => [...keyRow(level).querySelectorAll('button')].find((el) => el.textContent.includes(label));
const expectConsoleIndicator = (state, label, ledClass) => {
  const status = document.querySelector('.console-status-text');
  assert.equal(status?.dataset.connectionState, state);
  assert.equal(status?.textContent, label);
  assert.ok(document.querySelector(`.shell-controls .${ledClass}`), `expected ${ledClass} LED`);
};

test('SSH indicator follows connection events and marks a lost event stream red', async () => {
  const { source, setBackendSSHState, delayNextStatusRefresh, releaseStatusRefresh } = await startSSHBackend();
  expectConsoleIndicator('connected', 'ПОДКЛЮЧЕНО', 'led-green');
  await act(async () => source.emit('ssh:status', { state: 'connecting' }));
  expectConsoleIndicator('connecting', 'ПОДКЛЮЧЕНИЕ', 'led-amber');
  await act(async () => source.emit('ssh:status', { state: 'connected', user: 'bandit0' }));
  expectConsoleIndicator('connected', 'ПОДКЛЮЧЕНО', 'led-green');
  await act(async () => source.onerror());
  expectConsoleIndicator('disconnected', 'НЕТ СВЯЗИ', 'led-red');
  setBackendSSHState('disconnected');
  delayNextStatusRefresh();
  await act(async () => source.open());
  await settle();
  expectConsoleIndicator('connecting', 'ПРОВЕРКА', 'led-amber');
  releaseStatusRefresh();
  await settle();
  expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');

  await act(async () => source.onerror());
  delayNextStatusRefresh();
  await act(async () => source.open());
  await settle();
  expectConsoleIndicator('connecting', 'ПРОВЕРКА', 'led-amber');
  await act(async () => source.emit('ssh:status', { state: 'connected', user: 'bandit0' }));
  expectConsoleIndicator('connecting', 'ПРОВЕРКА', 'led-amber');
  releaseStatusRefresh();
  await settle();
  expectConsoleIndicator('connected', 'ПОДКЛЮЧЕНО', 'led-green');
  await act(async () => source.onerror());
  delayNextStatusRefresh();
  await act(async () => source.open());
  await settle();
  expectConsoleIndicator('connecting', 'ПРОВЕРКА', 'led-amber');
  await act(async () => source.onerror());
  await settle();
  expectConsoleIndicator('disconnected', 'НЕТ СВЯЗИ', 'led-red');
  releaseStatusRefresh();
  await settle();
  expectConsoleIndicator('disconnected', 'НЕТ СВЯЗИ', 'led-red');
  await act(async () => source.open());
  await settle();
  await act(async () => source.emit('ssh:status', { state: 'error', message: 'connection failed' }));
  expectConsoleIndicator('error', 'ОШИБКА', 'led-red');
});

test('Git running and finished stay green while errors and disconnects turn red', async () => {
  const { source, terminal } = await startSSHBackend();
  await moveToGitLevel(terminal);
  expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
  await edit(document.querySelector('input[aria-label="Пароль Git"]'), 'git-password');
  await click(button('СОХРАНИТЬ В ПАМЯТИ'));
  await settle();
  expectConsoleIndicator('connected', 'ПОДКЛЮЧЕНО', 'led-green');
  await act(async () => source.emit('git:status', { state: 'running' }));
  expectConsoleIndicator('connected', 'ВЫПОЛНЯЕТСЯ', 'led-green');
  await act(async () => source.emit('git:status', { state: 'finished' }));
  expectConsoleIndicator('connected', 'ПОДКЛЮЧЕНО', 'led-green');
  await act(async () => source.emit('git:status', { state: 'error', message: 'command failed' }));
  expectConsoleIndicator('error', 'ОШИБКА', 'led-red');
  await act(async () => source.emit('git:status', { state: 'disconnected' }));
  expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
});

test('Git connect shows pending, then connected only after completion', async () => {
  const { source, terminal, requests, releaseGitConnect } = await startSSHBackend({ delayGitConnect: true });
  await moveToGitLevel(terminal);
  await edit(document.querySelector('input[aria-label="Пароль Git"]'), 'git-password');
  await click(button('СОХРАНИТЬ В ПАМЯТИ'));
  await settle();
  expectConsoleIndicator('connecting', 'ПОДКЛЮЧЕНИЕ', 'led-amber');
  assert.deepEqual(requests.filter(({ path }) => path === '/api/git/connect'), [{ path: '/api/git/connect', body: { levelId: 28, password: 'git-password' } }]);
  await act(async () => source.emit('git:status', { state: 'connected' }));
  releaseGitConnect();
  await settle();
  expectConsoleIndicator('connected', 'ПОДКЛЮЧЕНО', 'led-green');
});

test('failed Git connect stays red and selected mode controls the indicator', async () => {
  const { source, terminal } = await startSSHBackend({ gitConnectError: true });
  await moveToGitLevel(terminal);
  await edit(document.querySelector('input[aria-label="Пароль Git"]'), 'wrong-password');
  await click(button('СОХРАНИТЬ В ПАМЯТИ'));
  await settle();
  expectConsoleIndicator('error', 'ОШИБКА', 'led-red');
  assert.equal(document.querySelector('[role="alert"].terminal-error').textContent, 'git login failed');
  await click(document.querySelector('.terminal-mode-buttons button:nth-child(1)'));
  expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
  await click(document.querySelector('.terminal-mode-buttons button:nth-child(2)'));
  expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
  await act(async () => source.emit('git:status', { state: 'disconnected' }));
  expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
});

test('cancelled Git connect cannot restore green after switching or disconnecting', async () => {
  for (const action of ['switch', 'disconnect']) {
    localStorage.clear();
    const { terminal, releaseGitConnect, hasGitPassword } = await startSSHBackend({ delayGitConnect: true });
    await moveToGitLevel(terminal);
    await edit(document.querySelector('input[aria-label="Пароль Git"]'), 'git-password');
    await click(button('СОХРАНИТЬ В ПАМЯТИ'));
    await settle();
    expectConsoleIndicator('connecting', 'ПОДКЛЮЧЕНИЕ', 'led-amber');
    await click(action === 'switch' ? document.querySelector('.terminal-mode-buttons button:nth-child(1)') : button('ОТКЛЮЧИТЬ'));
    await settle();
    expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
    assert.equal(hasGitPassword(), false, 'the first disconnect completes before the delayed connect response');
    if (action === 'switch') {
      await click(document.querySelector('.terminal-mode-buttons button:nth-child(2)'));
      expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
    }
    assert.ok(button('СОХРАНИТЬ В ПАМЯТИ').disabled, 'new Git connects wait for the pending request and cleanup');
    releaseGitConnect();
    await settle();
    assert.equal(hasGitPassword(), false, 'a final disconnect clears the password stored by the late connect');
    expectConsoleIndicator('disconnected', 'ОТКЛЮЧЕНО', 'led-red');
    assert.equal(button('СОХРАНИТЬ В ПАМЯТИ').disabled, false, `a new Git connect is allowed after ${action} cleanup; ${document.querySelector('.terminal-git-auth')?.textContent}`);
    await act(async () => root.unmount());
    root = null;
  }
});

test('keys start as 34 locked rows with visible disabled actions', async () => {
  await mount();
  await click(button('Ключи'));
  assert.equal(document.querySelectorAll('[data-key-level]').length, 34);
  assert.match(document.body.textContent, /ОТКРЫТО 0\/34/);
  for (let level=0;level<34;level++) {
    assert.match(keyRow(level).textContent,/Заблокирован/);
    assert.match(keyRow(level).textContent,/\*{8}/);
    assert.ok([...keyRow(level).querySelectorAll('button')].every((el)=>el.disabled));
  }
  assert.equal(button('Ключи').getAttribute('aria-pressed'),'true');
  assert.ok(button('Экспортировать').disabled);
});

test('valid key events persist one row, mark its task, and reject malformed entries', async () => {
  const {source}=await startSSHBackend();
  await click(button('Ключи'));
  for (const entry of [null,{},keyFixture(-1),keyFixture(34),{...keyFixture(1),username:'root'}, {...keyFixture(1),password:''}, {...keyFixture(1),password:'bad\nvalue'}, {...keyFixture(1),level:1.5}]) {
    await act(async()=>source.emit('keys:unlocked',entry));
  }
  assert.match(document.body.textContent,/ОТКРЫТО 0\/34/);
  await act(async()=>source.emit('keys:unlocked',keyFixture(0)));
  const afterInitialLogin=JSON.parse(localStorage.getItem('otw_bandit_completed'));
  assert.equal(afterInitialLogin['0'],true);
  assert.notEqual(afterInitialLogin['0-1'],true,'the bandit0 login key does not complete task 0→1');
  await act(async()=>source.emit('keys:unlocked',keyFixture(1)));
  assert.match(document.body.textContent,/ОТКРЫТО 2\/34/);
  assert.ok(keyRow(0).textContent.includes(keyFixture(0).password));
  assert.ok(keyRow(1).textContent.includes(keyFixture(1).password));
  assert.match(keyRow(2).textContent,/Заблокирован/);
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[keyFixture(0),keyFixture(1)]);
  assert.equal(JSON.parse(localStorage.getItem('otw_bandit_completed'))['0-1'],true);
});

test('saved keys restore valid rows and empty snapshots do not erase them', async () => {
  localStorage.setItem('otw_bandit_unlocked_keys',JSON.stringify([keyFixture(3),{...keyFixture(4),password:'bad\u007fvalue'}]));
  const {source,requests}=await startSSHBackend();
  await click(button('Ключи'));
  assert.match(document.body.textContent,/ОТКРЫТО 1\/34/);
  assert.ok(keyRow(3).textContent.includes(keyFixture(3).password));
  assert.match(keyRow(4).textContent,/Заблокирован/);
  await act(async()=>source.open()); await settle();
  assert.equal(requests.filter(({path})=>path==='/api/keys').length,2);
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[keyFixture(3)]);
});

test('snapshot merges confirmed keys and key 33 completes only 32 to 33', async () => {
  await startSSHBackend({snapshot:[keyFixture(0),keyFixture(33)]});
  const completed=JSON.parse(localStorage.getItem('otw_bandit_completed'));
  assert.equal(completed['0'],true);
  assert.equal(completed['32-33'],true);
  assert.notEqual(completed['33-34'],true);
  await click(button('Ключи'));
  assert.match(document.body.textContent,/ОТКРЫТО 2\/34/);
});

test('vault credential takes priority over terminal selection on Next', async () => {
  const {terminal,source,requests}=await startSSHBackend({completedLevels:{'1-2':true}});
  await moveToLevelOne(terminal);
  await act(async()=>source.emit('keys:unlocked',keyFixture(2)));
  terminal.selection='different-selection-fixture';
  await click(button('Следующий уровень →')); await settle();
  assert.deepEqual(requests.filter(({path})=>path==='/api/ssh/connect'),[{path:'/api/ssh/connect',body:{username:'bandit2',password:keyFixture(2).password}}]);
  assert.equal(terminal.output.includes(keyFixture(2).password),false);
});

test('key copying reports success in status and terminal and obeys sound preference', async () => {
  localStorage.setItem('otw_bandit_sound','false');
  let writes=[],sounds=0;
  dom.window.AudioContext=class {constructor(){ sounds++; }};
  Object.defineProperty(navigator,'clipboard',{value:{writeText:async(text)=>writes.push(text)}});
  const {source,terminal}=await startSSHBackend();
  await act(async()=>source.emit('keys:unlocked',keyFixture(1)));
  await click(button('Ключи'));
  await click(rowAction(1,'Копировать'));
  assert.deepEqual(writes,[keyFixture(1).password]);
  assert.ok([...document.querySelectorAll('[role="status"]')].some((el)=>/скопирован/i.test(el.textContent)));
  assert.match(terminal.output,/скопирован/i);
  assert.equal(terminal.output.includes(keyFixture(1).password),false);
  assert.equal(sounds,0);
  await click(button('SND: OFF'));
  const before=sounds;
  await click(rowAction(1,'Копировать'));
  assert.ok(sounds>before);
});

test('export includes only unlocked entries and a txt filename', async () => {
  const {source}=await startSSHBackend();
  await act(async()=>source.emit('keys:unlocked',keyFixture(2)));
  await click(button('Ключи'));
  let exported,filename;
  dom.window.URL.createObjectURL=(blob)=>{ exported=blob; return 'blob:key-fixture'; };
  dom.window.URL.revokeObjectURL=()=>{};
  dom.window.HTMLAnchorElement.prototype.click=function(){ filename=this.download; };
  await click(button('Экспортировать'));
  assert.match(filename,/\.txt$/);
  const text=await new Promise((resolve)=>{const reader=new dom.window.FileReader(); reader.onload=()=>resolve(reader.result); reader.readAsText(exported);});
  assert.equal(text,`bandit2: ${keyFixture(2).password}\n`);
});

test('reset cancellation and API failure preserve keys, successful reset removes only keys', async () => {
  for (const resetError of [true,false]) {
    localStorage.setItem('otw_bandit_notes',JSON.stringify('keep-notes'));
    const {source,requests}=await startSSHBackend({resetError});
    await act(async()=>source.emit('keys:unlocked',keyFixture(1)));
    await click(button('Ключи'));
    dom.window.confirm=()=>false;
    await click(exactButton('Сбросить…'));
    assert.equal(requests.some(({path})=>path==='/api/keys/reset'),false);
    assert.ok(localStorage.getItem('otw_bandit_unlocked_keys'));
    const previous=storageEntries().filter(([key])=>key!=='otw_bandit_unlocked_keys');
    dom.window.confirm=()=>true;
    await click(exactButton('Сбросить…')); await settle();
    assert.equal(localStorage.getItem('otw_bandit_unlocked_keys')!==null,resetError);
    assert.deepEqual(storageEntries().filter(([key])=>key!=='otw_bandit_unlocked_keys'),previous);
    await act(async()=>root.unmount()); root=null; localStorage.clear();
  }
});

test('full progress reset cancellation is inert and server reset preserves notes and settings', async () => {
  const progress = { version: 1, activeLevelId: '2-3', completed: { '2-3': true }, revision: 0 };
  localStorage.setItem('otw_bandit_notes', JSON.stringify('keep-notes'));
  localStorage.setItem('otw_bandit_sound', JSON.stringify(false));
  const backend = await startSSHBackend({ progress, snapshot: [keyFixture(2)] });
  await settle();
  dom.window.confirm = () => false;
  await click(button('Сбросить прогресс'));
  assert.equal(backend.requests.some(({ path }) => path === '/api/progress/reset'), false);
  assert.equal(document.body.textContent.includes('02/34'), true);
  dom.window.confirm = () => true;
  await click(button('Сбросить прогресс')); await settle();
  assert.deepEqual(backend.requests.find(({ path }) => path === '/api/progress/reset')?.body, {});
  assert.equal(document.body.textContent.includes('00/34'), true);
  assert.ok([...document.querySelectorAll('[role="alert"]')].some((el) => el.textContent === 'Прогресс и игровые ключи сброшены.'));
  assert.equal(backend.currentKeys().length, 0);
  await click(button('Ключи'));
  assert.ok(document.body.textContent.includes('ОТКРЫТО 0/34'));
  assert.equal(localStorage.getItem('otw_bandit_notes'), JSON.stringify('keep-notes'));
  assert.equal(localStorage.getItem('otw_bandit_sound'), JSON.stringify(false));
  assert.ok(backend.requests.some(({ path, body }) => path === '/api/progress' && body?.revision === 1 && body?.activeLevelId === '0'));
});

test('progress reset 409 shows the reason and keeps the current progress and keys', async () => {
  const progress = { version: 1, activeLevelId: '2-3', completed: { '1-2': true, '2-3': true }, revision: 0 };
  const key = keyFixture(2);
  const backend = await startSSHBackend({ progress, snapshot: [key], progressResetError: true });
  await settle();
  dom.window.confirm = () => true;
  await click(button('Сбросить прогресс')); await settle();
  assert.ok([...document.querySelectorAll('[role="alert"]')].some((el) => el.textContent.includes('disconnect first')));
  assert.equal(document.body.textContent.includes('02/34'), true);
  assert.equal(backend.currentProgress().activeLevelId, '2-3');
  assert.equal(backend.currentProgress().revision, 0);
  assert.equal(backend.currentKeys().length, 1);
});

test('preview progress reset clears only its own progress and local keys', async () => {
  localStorage.setItem('otw_bandit_active_level', JSON.stringify('2-3'));
  localStorage.setItem('otw_bandit_completed', JSON.stringify({ '0': true, '2-3': true }));
  localStorage.setItem('otw_bandit_unlocked_keys', JSON.stringify([keyFixture(2)]));
  localStorage.setItem('otw_bandit_notes', JSON.stringify('keep-notes'));
  localStorage.setItem('otw_bandit_crt', JSON.stringify('high'));
  await mount(); await settle();
  dom.window.confirm = () => true;
  await click(button('Сбросить прогресс')); await settle();
  assert.equal(JSON.parse(localStorage.getItem('otw_bandit_active_level')), '0');
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_completed')), { '0': false });
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'), null);
  assert.equal(localStorage.getItem('otw_bandit_notes'), JSON.stringify('keep-notes'));
  assert.equal(localStorage.getItem('otw_bandit_crt'), JSON.stringify('high'));
  assert.equal(document.body.textContent.includes('00/34'), true);
});

test('literal SSH command is inserted only into a connected SSH shell', async () => {
  const {source,terminal,requests}=await startSSHBackend();
  await act(async()=>source.emit('keys:unlocked',keyFixture(1)));
  await click(button('Ключи'));
  await click(rowAction(1,'Вставить SSH'));
  await new Promise((resolve)=>setTimeout(resolve,15)); await settle();
  const command='ssh bandit1@bandit.labs.overthewire.org -p 2220';
  assert.deepEqual(terminal.pasted,[command]);
  assert.deepEqual(requests.filter(({path})=>path==='/api/ssh/input').map(({body})=>body.data),[command]);
  await act(async()=>source.emit('ssh:status',{state:'disconnected'}));
  await click(button('Ключи')); await click(rowAction(1,'Вставить SSH'));
  assert.equal(terminal.pasted.length,1);
  assert.ok([...document.querySelectorAll('[role="status"]')].some((el)=>/подключ/i.test(el.textContent)));
  await moveToGitLevel(terminal);
  await click(button('Ключи')); await click(rowAction(1,'Вставить SSH'));
  assert.equal(terminal.pasted.length,1);
  assert.equal(requests.some(({path})=>path==='/api/git/command'),false);
});

test('a snapshot started before reset cannot restore cleared keys', async () => {
  let releaseSnapshot;
  const snapshot=new Promise((resolve)=>{releaseSnapshot=resolve;});
  const {source}=await startSSHBackend({snapshot});
  await act(async()=>source.emit('keys:unlocked',keyFixture(1)));
  await click(button('Ключи'));
  dom.window.confirm=()=>true;
  await click(exactButton('Сбросить…')); await settle();
  await act(async()=>releaseSnapshot([keyFixture(3)])); await settle();
  assert.match(document.body.textContent,/ОТКРЫТО 0\/34/);
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'),null);
});

test('unlock after backend reset survives reconciliation without interrupting live output', async () => {
  let releaseResponse;
  const responseGate = new Promise((resolve) => { releaseResponse = resolve; });
  const backend = await startSSHBackend({ beforeResetResponse: async ({ setSnapshot, source }) => {
    setSnapshot([keyFixture(3)]);
    source.emit('keys:unlocked',keyFixture(3));
    source.emit('ssh:data','LIVE-SSH-DURING-RESET');
    source.emit('git:output','LIVE-GIT-DURING-RESET');
    source.emit('git:status',{state:'finished',message:'LIVE-GIT-STATUS'});
    source.emit('ssh:status',{state:'connected',user:'bandit0'});
    await responseGate;
  }});
  await act(async()=>backend.source.emit('keys:unlocked',keyFixture(1)));
  await click(button('Ключи')); dom.window.confirm=()=>true;
  await click(exactButton('Сбросить…'));
  assert.equal(backend.terminal.output.includes('LIVE-SSH-DURING-RESET'),true,'SSH output must remain live while POST is pending');
  assert.equal(backend.terminal.output.includes('LIVE-GIT-DURING-RESET'),true,'Git output must remain live while POST is pending');
  assert.equal(backend.terminal.output.includes('LIVE-GIT-STATUS'),true,'Git status must remain live while POST is pending');
  assert.equal(backend.terminal.options.disableStdin,false,'reset must not disable shell input');
  await act(async()=>releaseResponse()); await settle();
  const saved=JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')) || [];
  assert.equal(saved.some(({level})=>level===3),true,'newly confirmed backend key must survive reset');
  assert.equal(saved.some(({level})=>level===1),false,'pre-reset key must be cleared');
  assert.notEqual(backend.source.closed,true);
  assert.equal(backend.currentSource,backend.source);
});

test('queued pre-reset key reconciles current snapshot while terminal callbacks remain live', async () => {
  const backend = await startSSHBackend();
  await act(async()=>backend.source.emit('keys:unlocked',keyFixture(1)));
  await click(button('Ключи')); dom.window.confirm=()=>true;
  await click(exactButton('Сбросить…')); await settle();
  await act(async()=>{
    backend.source.deliver('keys:unlocked',keyFixture(1));
    backend.source.emit('ssh:data','LIVE-TERMINAL-MARKER');
    backend.source.emit('ssh:status',{state:'connected',user:'bandit0'});
  });
  await settle();
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'),null,'removed key must not return from a stale event');
  assert.equal(backend.terminal.output.includes('LIVE-TERMINAL-MARKER'),true);
  assert.equal(backend.currentSource,backend.source);
  await act(async()=>backend.currentSource.emit('keys:unlocked',keyFixture(4)));
  await settle();
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[keyFixture(4)]);
});

test('key event payload is only a signal and the snapshot supplies the credential', async () => {
  const backend=await startSSHBackend();
  backend.setSnapshot([keyFixture(6)]);
  await act(async()=>backend.source.deliver('keys:unlocked',{...keyFixture(6),password:'untrusted-event-body-fixture'}));
  await settle();
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[keyFixture(6)]);
  assert.equal(backend.requests.filter(({path})=>path==='/api/keys').length,2);
});

test('overlapping key snapshots apply only the latest request result', async () => {
  const backend=await startSSHBackend();
  let releaseOlder;
  backend.setSnapshot(new Promise((resolve)=>{releaseOlder=resolve;}));
  await act(async()=>backend.source.deliver('keys:unlocked',keyFixture(6)));
  const current={...keyFixture(6),password:'latest-snapshot-fixture'};
  backend.setSnapshot([current]);
  await act(async()=>backend.source.deliver('keys:unlocked',keyFixture(6)));
  await settle();
  await act(async()=>releaseOlder([{...keyFixture(6),password:'older-snapshot-fixture'}]));
  await settle();
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[current]);
});

test('snapshot failure is visible without storing event payload or interrupting terminal output', async () => {
  const backend=await startSSHBackend();
  backend.setSnapshot(new Error('fixture snapshot failure'));
  await act(async()=>backend.source.deliver('keys:unlocked',keyFixture(7))); await settle();
  await click(button('Ключи'));
  assert.ok([...document.querySelectorAll('[role="status"]')].some((el)=>/Не удалось обновить архив/.test(el.textContent)));
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'),null);
  await act(async()=>backend.source.emit('ssh:data','LIVE-DURING-SNAPSHOT-FAILURE'));
  assert.equal(backend.terminal.output.includes('LIVE-DURING-SNAPSHOT-FAILURE'),true);
  backend.setSnapshot([keyFixture(7)]);
  await act(async()=>backend.source.deliver('keys:unlocked',keyFixture(7))); await settle();
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[keyFixture(7)]);
  assert.ok([...document.querySelectorAll('[role="status"]')].some((el)=>el.textContent==='Архив ключей обновлён.'));
});

test('post-reset snapshot failure distinguishes completed local clear from storage failure', async () => {
  const backend=await startSSHBackend({beforeResetResponse:({setSnapshot})=>setSnapshot(new Error('fixture snapshot failure'))});
  await act(async()=>backend.source.emit('keys:unlocked',keyFixture(1)));
  await click(button('Ключи')); dom.window.confirm=()=>true;
  await click(exactButton('Сбросить…')); await settle();
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'),null);
  const notices=[...document.querySelectorAll('[role="status"]')].map((el)=>el.textContent).join(' ');
  assert.equal(notices.includes('Ключи сброшены, но актуальный архив не получен.'),true);
  assert.equal(notices.includes('локальное хранилище ключей не очищено'),false);
  assert.equal(backend.currentSource,backend.source);
  backend.setSnapshot([]);
  await act(async()=>backend.source.open()); await settle();
  assert.ok([...document.querySelectorAll('[role="status"]')].some((el)=>el.textContent==='Архив ключей обновлён.'));
});

test('oversized key entries are rejected from storage snapshot and SSE while 256 ASCII bytes remain valid', async () => {
  const oversized = {...keyFixture(1),password:'x'.repeat(257)};
  localStorage.setItem('otw_bandit_unlocked_keys',JSON.stringify([oversized]));
  const {source}=await startSSHBackend({snapshot:[oversized]});
  await act(async()=>source.emit('keys:unlocked',{...oversized,level:2,username:'bandit2'}));
  await click(button('Ключи'));
  assert.equal(document.body.textContent.includes('ОТКРЫТО 0/34'),true,'oversized credentials must be rejected');
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'),null);
  const bounded={...keyFixture(3),password:'x'.repeat(256)};
  await act(async()=>source.emit('keys:unlocked',bounded));
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[bounded]);
});

test('key ingress enforces 256 UTF-8 bytes for multibyte passwords', async () => {
  const oversized={...keyFixture(1),password:'é'.repeat(129)};
  assert.equal(new TextEncoder().encode(oversized.password).byteLength,258);
  localStorage.setItem('otw_bandit_unlocked_keys',JSON.stringify([oversized]));
  const {source}=await startSSHBackend({snapshot:[{...oversized,level:2,username:'bandit2'}]});
  await act(async()=>source.emit('keys:unlocked',{...oversized,level:3,username:'bandit3'}));
  await settle();
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'),null,'all ingress must reject more than 256 UTF-8 bytes');
  const boundary={...keyFixture(4),password:'é'.repeat(128)};
  await act(async()=>source.emit('keys:unlocked',boundary)); await settle();
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_unlocked_keys')),[boundary]);
});

test('failed local key removal after backend reset reports partial failure and permits retry', async () => {
  const backend=await startSSHBackend();
  await act(async()=>backend.source.emit('keys:unlocked',keyFixture(1)));
  await click(button('Ключи'));
  let removalBlocked=true;
  const removeItem=dom.window.Storage.prototype.removeItem;
  dom.window.Storage.prototype.removeItem=function(key){
    if(key==='otw_bandit_unlocked_keys' && removalBlocked) throw new Error('storage removal denied');
    return removeItem.call(this,key);
  };
  dom.window.confirm=()=>true;
  await click(exactButton('Сбросить…')); await settle();
  assert.ok(localStorage.getItem('otw_bandit_unlocked_keys'),'failed removal must remain visible as incomplete');
  const notices=[...document.querySelectorAll('[role="status"]')].map((el)=>el.textContent).join(' ');
  assert.match(notices,/локальн.*хранилищ/i);
  assert.equal(notices.includes('Открытые ключи сброшены.'),false);
  assert.equal(exactButton('Сбросить…').disabled,false,'retry remains available');
  assert.equal(backend.currentSource,backend.source,'stream stays open even after local storage failure');
  removalBlocked=false;
  await click(exactButton('Сбросить…')); await settle();
  assert.equal(localStorage.getItem('otw_bandit_unlocked_keys'),null);
  assert.ok([...document.querySelectorAll('[role="status"]')].some((el)=>el.textContent==='Открытые ключи сброшены.'));
});

test('malformed event JSON and failed clipboard do not leak data or announce success', async () => {
  const {source,terminal}=await startSSHBackend();
  await act(async()=>source.listeners.get('keys:unlocked')({data:'not-json'}));
  await act(async()=>source.emit('keys:unlocked',keyFixture(2)));
  Object.defineProperty(navigator,'clipboard',{value:{writeText:async()=>{throw new Error('clipboard denied');}}});
  await click(button('Ключи')); await click(rowAction(2,'Копировать'));
  assert.ok([...document.querySelectorAll('[role="status"]')].some((el)=>/Не удалось скопировать/.test(el.textContent)));
  assert.equal(terminal.output.includes(keyFixture(2).password),false);
  assert.equal(terminal.output.includes('скопирован.'),false);
});

test('restored vault keys survive an unmount and remount without server confirmation being repeated', async () => {
  const {source}=await startSSHBackend();
  await act(async()=>source.emit('keys:unlocked',keyFixture(5)));
  await act(async()=>root.unmount()); root=null;
  await startSSHBackend();
  await click(button('Ключи'));
  assert.match(document.body.textContent,/ОТКРЫТО 1\/34/);
  assert.ok(keyRow(5).textContent.includes(keyFixture(5).password));
  assert.equal(keyRow(6).textContent.includes(keyFixture(5).password),false);
});

test('restores notes and preferences, and persists edits', async () => {
  localStorage.setItem('otw_bandit_notes', JSON.stringify('Сохранённая заметка'));
  localStorage.setItem('otw_bandit_crt', JSON.stringify('off'));
  localStorage.setItem('otw_bandit_sound', JSON.stringify(false));
  await mount();
  assert.ok(button('CRT: OFF'));
  assert.ok(button('SND: OFF'));
  assert.equal(button('Автопереход'), undefined);
  await click(button('Заметки'));
  const notes = document.querySelector('textarea');
  assert.equal(notes.value, 'Сохранённая заметка');
  assert.ok(notes.getAttribute('aria-label'));
  await edit(notes, 'bandit0: пароль найден');
  assert.equal(JSON.parse(localStorage.getItem('otw_bandit_notes')), 'bandit0: пароль найден');
  await click(button('SND: OFF'));
  assert.equal(JSON.parse(localStorage.getItem('otw_bandit_sound')), true);
});

test('rejects malformed saved state without crashing or enabling unknown modes', async () => {
  for (const key of ['completed', 'unlocked']) localStorage.setItem(`otw_bandit_${key}`, 'null');
  localStorage.setItem('otw_bandit_crt', JSON.stringify('invalid'));
  localStorage.setItem('otw_bandit_notes', JSON.stringify({ bad: true }));
  await mount();
  assert.ok(!document.querySelector('.crt-mode-invalid'));
  assert.ok(button('CRT: SOFT'));
  await click(button('Заметки'));
  assert.notEqual(document.querySelector('textarea').value, '[object Object]');
});

test('storage failure is visible and leaves current notes usable', async () => {
  dom.window.Storage.prototype.setItem = () => { throw new Error('quota'); };
  await mount();
  await click(button('Заметки'));
  await edit(document.querySelector('textarea'), 'важная заметка');
  assert.ok(document.querySelector('[role="alert"]'));
  assert.equal(document.querySelector('textarea').value, 'важная заметка');
});

test('browser preview explains that real terminal access requires the desktop app', async () => {
  await mount();
  assert.match(document.querySelector('.terminal-backend-notice').textContent, /BANDIT\.exe/);
  assert.ok(document.querySelector('.terminal-surface'), 'xterm surface is present');
  assert.equal(button('solve'), undefined, 'browser simulation shortcut is removed');
  assert.equal(button('Пароли'), undefined, 'preloaded password vault is removed');
});

test('reduced motion disables dynamic grain and responds to preference changes', async () => {
  media.matches = true;
  await mount();
  assert.ok(!document.querySelector('canvas'), 'no animated grain with reduced motion');
  media.matches = false;
  await act(async () => listener({ matches: false }));
  assert.ok(document.querySelector('canvas'));
});

test('next level moves only after the current stage is complete', async () => {
  const { source } = await startSSHBackend({ snapshot: [keyFixture(0)], completedLevels: { '0': false, '2-3': true } });
  assert.ok(button('Следующий уровень →'), 'confirmed bandit0 login completes the initial login stage');
  assert.equal(button('Этап пройден'), undefined);
  const terminal = document.querySelector('.terminal-surface');
  await click(button('Следующий уровень →'));
  assert.match(document.querySelector('.mission-stage-meta').textContent, /0 → 1/);
  const completed=JSON.parse(localStorage.getItem('otw_bandit_completed'));
  assert.equal(completed['0'],true);
  assert.notEqual(completed['0-1'],true,'navigation does not complete the puzzle stage');
  assert.equal(button('Следующий уровень →'),undefined,'the puzzle stage needs its own confirmed key');
  assert.equal(document.querySelector('.terminal-surface'), terminal, 'the terminal remains mounted across levels');
});

test('all 35 cards expose a goal and solution without hint UI', async () => {
  await mount();
  const cards = [...document.querySelectorAll('button')].filter((el) => /^\[\d{2}\]/.test(el.textContent.trim()));
  assert.equal(cards.length, 35);
  for (const [index, card] of cards.entries()) {
    await click(card);
    assert.ok([...document.querySelectorAll('.mission-goal')].map((el) => el.textContent).join('').trim(), 'card has a goal');
    assert.equal(button('Подсказка'), undefined);
    assert.equal(document.querySelector('#mission-hint-panel'), null);
    await click(button('Решение'));
    assert.ok(document.querySelector('#mission-solution-panel p').textContent.trim());
    assert.ok(button('Справочник команд'));
  }
});

test('goal glossary keeps criteria visible and opens one accessible tooltip with dismissal', async () => {
  await mount();
  await click(button('[02]'));
  const dash = document.querySelector('.mission-glossary-term');
  assert.equal(dash.textContent, '-', 'the literal filename remains visible in the original goal');
  dash.focus();
  await settle();
  assert.match(document.querySelector('[role="tooltip"]').textContent, /буквальное имя файла/, 'keyboard focus opens the tooltip');
  await click(dash);
  assert.ok(document.querySelector('[role="tooltip"]'), 'the first click pins a tooltip opened by focus');
  await click(dash);
  assert.equal(document.querySelector('[role="tooltip"]'), null, 'a later click toggles the pinned tooltip closed');
  await click(dash);
  document.dispatchEvent(new document.defaultView.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  await settle();
  assert.equal(document.querySelector('[role="tooltip"]'), null, 'Escape dismisses the tooltip');
  await click(dash);
  document.dispatchEvent(new document.defaultView.Event('pointerdown', { bubbles: true }));
  await settle();
  assert.equal(document.querySelector('[role="tooltip"]'), null, 'clicking elsewhere dismisses the tooltip');

  await click(button('[06]'));
  const goal = document.querySelector('.mission-goal');
  assert.match(goal.textContent, /ровно 1033 байта/);
  assert.ok([...goal.querySelectorAll('.mission-goal-emphasis')].some((item) => item.textContent.includes('1033 байта')), 'orange criterion emphasis remains intact');
  assert.ok([...goal.querySelectorAll('.mission-glossary-term')].some((term) => term.textContent === 'исполняемым'));
  assert.ok([...goal.querySelectorAll('.mission-glossary-term')].some((term) => term.textContent === 'человекочитаемый'));
  const inhere = [...goal.querySelectorAll('.mission-glossary-term')].find((term) => term.textContent.toLowerCase() === 'inhere');
  assert.ok(inhere, 'the literal inhere folder name is explained in context');
  inhere.focus();
  await settle();
  assert.match(document.querySelector('[role="tooltip"]').textContent, /буквальное имя папки/i);
});

test('command guide explains cat stdin and find negation once without dropping flags', async () => {
  await mount();
  await click(button('[06]'));
  const reference = document.querySelector('#mission-reference-panel');
  await click(button('Справочник команд'));
  await settle();
  const openGuide = async (command) => {
    const trigger = [...reference.querySelectorAll('button')].find((el) => el.textContent.trim().includes(command));
    await click(trigger);
    return reference.querySelector('.mission-command-guide');
  };
  const findGuide = await openGuide('find');
  assert.ok(findGuide.querySelector('.mission-command-syntax'));
  assert.ok(findGuide.querySelector('.mission-command-flag'));
  assert.match(findGuide.textContent, /Знак ! отрицает следующее условие/);
  assert.ok([...findGuide.querySelectorAll('li')].some((item) => item.textContent.includes('! -executable')));
  assert.equal([...findGuide.querySelectorAll('h3')].filter((heading) => heading.textContent.includes('Термин')).length, 1);
  await click(button('[02]'));
  const catReference = document.querySelector('#mission-reference-panel');
  await click(button('Справочник команд'));
  await settle();
  const catTrigger = [...catReference.querySelectorAll('button')].find((el) => el.textContent.trim().includes('cat'));
  await click(catTrigger);
  assert.match(catReference.querySelector('.mission-command-guide').textContent, /Особый аргумент `-` означает чтение со стандартного ввода/);
});

test('solution and command reference are exclusive and close on level change', async () => {
  await mount();
  const cards = [...document.querySelectorAll('button')].filter((el) => /^\[\d{2}\]/.test(el.textContent.trim()));
  assert.equal(button('Подсказка'), undefined);
  const referenceSummary = button('Справочник команд');
  const reference = document.querySelector('#mission-reference-panel');
  assert.equal(referenceSummary.tagName, 'BUTTON', 'the reference keeps a native keyboard-operable control');
  assert.equal(referenceSummary.getAttribute('aria-controls'), 'mission-reference-panel');
  assert.equal(document.getElementById(referenceSummary.getAttribute('aria-controls')).id, 'mission-reference-panel');
  await click(referenceSummary);
  await settle();
  assert.equal(reference.hidden, false);
  assert.equal(referenceSummary.getAttribute('aria-expanded'), 'true');
  assert.equal(button('Решение').getAttribute('aria-expanded'), 'false');
  await click(button('Решение'));
  await settle();
  assert.equal(document.querySelector('#mission-hint-panel'), null);
  assert.equal(reference.hidden, true);
  assert.equal(referenceSummary.getAttribute('aria-expanded'), 'false');
  assert.equal(document.querySelector('#mission-solution-panel').hidden, false);
  assert.equal(button('Решение').getAttribute('aria-expanded'), 'true');
  await click(referenceSummary);
  await settle();
  assert.equal(reference.hidden, false);
  assert.equal(document.querySelector('#mission-solution-panel').hidden, true);
  await click(referenceSummary);
  await settle();
  assert.equal(reference.hidden, true, 'the active reference control toggles closed');
  await click(button('Решение'));
  await click(cards.find((el) => el.textContent.trim().startsWith('[02]')));
  assert.equal(button('Подсказка'), undefined);
  assert.equal(button('Решение').getAttribute('aria-expanded'), 'false');
  assert.equal(document.querySelector('#mission-hint-panel'), null);
  assert.equal(document.querySelector('#mission-solution-panel').hidden, true);
  assert.equal(reference.hidden, true);
});

test('opening prose does not send commands and explicit insert and EXEC use solutionCmd', async () => {
  const { requests, terminal } = await startSSHBackend({ snapshot: [keyFixture(0)] });
  const cards = [...document.querySelectorAll('button')].filter((el) => /^\[\d{2}\]/.test(el.textContent.trim()));
  await click(cards.find((el) => el.textContent.trim().startsWith('[01]')));
  assert.equal(button('Подсказка'), undefined);
  await click(button('Решение'));
  assert.equal((terminal.pasted || []).length, 0, 'opening disclosures does not paste terminal input');
  assert.equal(requests.some(({ path }) => path === '/api/ssh/input'), false, 'opening disclosures does not call SSH input');
  await click(button('Вставить команду'));
  await act(async () => new Promise((resolve) => setTimeout(resolve, 40)));
  assert.ok(terminal.pasted.includes('cat readme'), 'explicit insert uses this card solutionCmd');
  assert.equal(terminal.pasted.filter((command) => command === 'cat readme').length, 1, 'the solution panel keeps its explicit insert action');
});

test('Git card buttons queue separate commands for the selected stage', async () => {
  const { requests, terminal } = await startSSHBackend({ completedLevels: { '27-28': true, '31-32': true } });
  await moveToGitLevel(terminal);
  const cards = [
    [28, ['git clone', 'cat README']],
    [32, ['cat README.md', 'write key.txt "May I come in?"', 'git add -f key.txt', 'git commit -m key', 'git push origin master']]
  ];
  const gitCommands = () => requests.filter(({ path }) => path === '/api/git/command').map(({ body }) => body);

  for (const [levelId, commands] of cards) {
    if (levelId === 32) {
      await click(button('[32]'));
      await settle();
    }
    const chips = [...document.querySelectorAll('.mission-quick-actions button')];
    assert.deepEqual(chips.map((chip) => chip.getAttribute('aria-label')), commands.map((command) => `Вставить команду: ${command}`));
    const previousCount = gitCommands().length;
    for (const [index, chip] of chips.entries()) {
      await click(chip);
      await act(async () => new Promise((resolve) => setTimeout(resolve, 40)));
      assert.equal(gitCommands().length, previousCount + index);
      await act(async () => terminal.send('\r'));
      await settle();
    }
    assert.deepEqual(gitCommands().slice(previousCount), commands.map((command) => ({ levelId, command })));
    await click(button('Решение'));
    assert.equal(document.querySelector('#mission-solution-panel > code').textContent, commands.at(-1));
  }
});

test('disclosure controls use native buttons and correctly linked accessible state', async () => {
  await mount();

  const solution = button('Решение');
  assert.equal(button('Подсказка'), undefined);
  for (const control of [solution]) {
    const panel = document.getElementById(control.getAttribute('aria-controls'));
    assert.ok(panel, 'aria-controls points to a panel');
    assert.equal(panel.getAttribute('aria-labelledby'), control.id);
    assert.equal(control.getAttribute('aria-expanded'), 'false');
    assert.equal(panel.hidden, true);
  }
  await click(solution);
  assert.equal(solution.getAttribute('aria-expanded'), 'true');
  assert.equal(document.querySelector('#mission-hint-panel'), null);
  assert.equal(document.querySelector('#mission-solution-panel').hidden, false);
});

test('output-unlocked target key marks the active task and shows Next without selection or login', async () => {
  const { source, requests, terminal } = await startSSHBackend({ snapshot: [keyFixture(0)] });
  await click(button('[01]'));
  await settle();
  assert.equal(button('Следующий уровень →'),undefined);
  assert.equal(document.querySelector('.next-level-pulse'),null,'the next-level pulse is hidden before completion');
  assert.notEqual(JSON.parse(localStorage.getItem('otw_bandit_completed'))['0-1'],true);
  assert.equal(summary('Примеры команд'),undefined,'task commands are visible in the main briefing');
  assert.ok(button('> cat readme')?.disabled,'quick commands stay disabled until the selected task is completed');
  assert.match(button('> cat readme').title,/после прохождения уровня/i);
  assert.match(document.querySelector('.mission-quick-actions-note')?.textContent,/после прохождения уровня/i);
  assert.equal((terminal.pasted || []).length,0,'disabled quick commands do not paste');
  assert.equal(button('Подсказка'), undefined);
  assert.equal(document.querySelector('#mission-hint-panel'), null);
  terminal.selection='';
  await act(async()=>source.emit('keys:unlocked',keyFixture(1)));
  await settle();
  assert.equal(JSON.parse(localStorage.getItem('otw_bandit_completed'))['0-1'],true);
  assert.equal(document.querySelector('.mission-complete strong')?.textContent,'КЛЮЧ НАЙДЕН · ЭТАП ЗАСЧИТАН');
  assert.match(document.querySelector('.mission-complete span:last-child')?.textContent,/Пароль сохранён в архиве/);
  assert.equal(button('> cat readme').disabled,false,'backend-confirmed completion activates quick commands');
  assert.equal(document.querySelector('.mission-quick-actions-note'),null);
  await click(button('> cat readme'));
  await act(async()=>new Promise((resolve)=>setTimeout(resolve,40)));
  assert.equal(terminal.pasted.at(-1),'cat readme','an activated quick command inserts into the terminal');
  assert.equal(requests.some(({path})=>path==='/api/ssh/input'),false,'pasting does not execute the command');
  await click(button('[01]'));
  assert.equal(button('> cat readme').disabled,false,'a previously completed task keeps its commands enabled when revisited');
  assert.ok(button('Следующий уровень →'));
  assert.ok(button('Следующий уровень →').classList.contains('next-level-pulse'),'the available next-level button pulses');
  const appStyles = [...document.querySelectorAll('style')].map((style) => style.textContent).join('\n');
  assert.match(appStyles, /@keyframes next-level-pulse/);
  assert.doesNotMatch(appStyles, /\.next-level-pulse:hover[\s\S]*?animation-play-state:\s*paused/,'hover does not stop the pulse');
  assert.match(appStyles, /50%\s*\{\s*background-color:\s*#ffd5ad;\s*box-shadow:\s*0 0 16px 3px rgba\(255, 85, 0, 0\.72\)/,'pulse reaches a clearly warm, visible color');
  assert.match(appStyles, /\.mission-navigation \.next-level-pulse:hover \{ color: #000000; background-color: #ffd5ad; \}/,'hover keeps dark text on a light background when motion is disabled');
  assert.match(appStyles, /prefers-reduced-motion: reduce[\s\S]*?\.mission-navigation \.next-level-pulse \{ animation: none !important; \}/);
  assert.equal(document.querySelector('.mission-footer').children.length,3);
  assert.equal(requests.some(({path})=>path==='/api/ssh/connect'),false,'the button appears before any Next click or new login');
});

test('browsing levels and editing notes do not mark tasks complete', async () => {
  await mount();
  await click(button('[02]'));
  await click(button('← Назад'));
  await click(button('Заметки'));
  await edit(document.querySelector('textarea'), 'проверка пароля');
  assert.match(document.querySelector('.mission-stage-meta').textContent, /0 → 1/);
  assert.deepEqual(JSON.parse(localStorage.getItem('otw_bandit_completed')), { '0': false });
  assert.equal(button('Следующий уровень →'),undefined,'the selected task remains locked until its target key is confirmed');
});

test('final outro has no next action and is excluded from progress', async () => {
  localStorage.setItem('otw_bandit_completed', JSON.stringify({ '33-34': true }));
  await mount();
  await click(button('[34]'));
  assert.equal(button('Следующий уровень →'), undefined);
  const progress = [...document.querySelectorAll('span')].find((el) => /\d+\/\d+/.test(el.textContent));
  assert.equal(progress.textContent.trim(), '00/34 [000%]');
});

test('next-level selection logs into the target user once without exposing or storing the password', async () => {
  const { requests, source, terminal } = await startSSHBackend({completedLevels:{'1-2':true,'2-3':true}});
  await moveToLevelOne(terminal);
  const password = 'cobalt-owl-42';
  terminal.selection = password;
  await click(button('Следующий уровень →'));
  await settle();
  const connects = requests.filter(({ path }) => path === '/api/ssh/connect');
  assert.ok(connects.length, `requests: ${requests.map(({ path }) => path).join(', ')}; terminal: ${terminal.output}`);
  assert.deepEqual(connects, [{ path: '/api/ssh/connect', body: { username: 'bandit2', password } }]);
  assert.match(document.querySelector('[role="status"].terminal-state').textContent, /ПОДКЛЮЧЕНИЕ/);
  assert.equal(terminal.output.includes(password), false);
  assert.equal(storageEntries().some(([key, value]) => key.includes(password) || value.includes(password)), false);
  assert.equal(terminal.selection, '', 'selection is cleared after it is captured');
  await act(async () => source.emit('ssh:status', { state: 'disconnected' }));
  assert.equal(terminal.output.includes('login as: bandit2'), false, 'a delayed old-session event is not treated as an auth result');
  await act(async () => source.emit('ssh:status', { state: 'connecting', user: 'bandit2' }));
  await act(async () => source.emit('ssh:status', { state: 'connected', user: 'bandit2' }));
  await settle();
  await click(button('Следующий уровень →'));
  await settle();
  assert.equal(requests.filter(({ path }) => path === '/api/ssh/connect').length, 1, 'a later transition does not reuse the password');
});

test('Git terminal selection is preserved through Next mode switch and sent to the target user', async () => {
  const { requests, terminal } = await startSSHBackend({completedLevels:{'27-28':true}});
  await moveToGitLevel(terminal);
  assert.match(terminal.output, /GIT \/\/ локальная рабочая копия/);
  terminal.selection = 'git-found-password';
  await click(button('Следующий уровень →'));
  await settle();
  assert.deepEqual(requests.filter(({ path }) => path === '/api/ssh/connect'), [{
    path: '/api/ssh/connect', body: { username: 'bandit28', password: 'git-found-password' }
  }]);
});

test('missing, empty, multiline and oversized selections keep the manual password prompt', async () => {
  for (const selection of [null, '', 'first\nsecond', 'x'.repeat(257), 'é'.repeat(129)]) {
    localStorage.clear();
    const { requests, terminal } = await startSSHBackend({completedLevels:{'1-2':true}});
    await moveToLevelOne(terminal);
    terminal.selection = selection ?? '';
    await click(button('Следующий уровень →'));
    await settle();
    assert.equal(requests.some(({ path }) => path === '/api/ssh/connect'), false);
    assert.match(terminal.output, /login as: bandit2/);
    assert.equal(terminal.output.includes('first\nsecond'), false);
    await act(async () => root.unmount());
    root = null;
  }
});

test('SSH status rejection clears the one-time candidate and returns to manual input', async () => {
  const { requests, source, terminal } = await startSSHBackend({completedLevels:{'1-2':true,'2-3':true}});
  await moveToLevelOne(terminal);
  const password = 'rejected-secret';
  terminal.selection = password;
  await click(button('Следующий уровень →'));
  await settle();
  await act(async () => source.emit('ssh:status', { state: 'connecting', user: 'bandit2' }));
  await act(async () => source.emit('ssh:status', { state: 'error', message: `Authentication failed: ${password}` }));
  await settle();
  assert.match(terminal.output, /login as: bandit2/);
  assert.equal(terminal.output.includes(password), false);
  assert.equal(document.body.textContent.includes(password), false);
  assert.equal(requests.filter(({ path }) => path === '/api/ssh/connect').length, 1);
  await act(async () => {
    terminal.send('\r');
    terminal.send('manual-fallback-password\r');
  });
  await settle();
  assert.deepEqual(requests.filter(({ path }) => path === '/api/ssh/connect').at(-1), {
    path: '/api/ssh/connect', body: { username: 'bandit2', password: 'manual-fallback-password' }
  });
  assert.equal(terminal.output.includes('manual-fallback-password'), false);
  await click(button('Следующий уровень →'));
  await settle();
  assert.equal(requests.filter(({ path }) => path === '/api/ssh/connect').length, 2);
});

test('SSE error before connecting clears the candidate and restores manual input', async () => {
  const { requests, source, terminal } = await startSSHBackend({completedLevels:{'1-2':true}});
  await moveToLevelOne(terminal);
  terminal.selection = 'early-error-secret';
  await click(button('Следующий уровень →'));
  await settle();
  await act(async () => source.emit('ssh:status', { state: 'error', message: 'Authentication rejected' }));
  await settle();
  assert.match(terminal.output, /login as: bandit2/);
  assert.equal(terminal.output.includes('early-error-secret'), false);
  assert.equal(requests.filter(({ path }) => path === '/api/ssh/connect').length, 1);
  assert.equal(terminal.options.disableStdin, false);
});

test('disconnect failure with confirmed disconnected status returns to manual input', async () => {
  const { requests, terminal } = await startSSHBackend({ disconnectError: true, completedLevels:{'0':true} });
  terminal.selection = 'disconnect-failed-secret';
  await click(button('Следующий уровень →'));
  await settle();
  assert.match(terminal.output, /login as: bandit0/);
  assert.equal(document.querySelector('[role="status"].terminal-state').textContent, 'НЕ ПОДКЛЮЧЕНО');
  assert.equal(terminal.options.disableStdin, false);
  assert.equal(requests.some(({ path }) => path === '/api/ssh/connect'), false);
  assert.equal(terminal.output.includes('disconnect-failed-secret'), false);
});

test('disconnect failure restores a session only after status confirms it is connected', async () => {
  const { requests, terminal } = await startSSHBackend({ disconnectError: true, disconnectStateOnError: 'connected', completedLevels:{'0':true} });
  terminal.selection = 'connected-session-secret';
  await click(button('Следующий уровень →'));
  await settle();
  assert.equal(document.querySelector('[role="status"].terminal-state').textContent, 'SSH · bandit0');
  assert.equal(terminal.options.disableStdin, false);
  assert.equal(terminal.output.includes('login as: bandit0'), false);
  assert.equal(terminal.output.includes('connected-session-secret'), false);
  assert.equal(requests.some(({ path }) => path === '/api/ssh/connect'), false);
});

test('disconnect and status timeouts keep unknown SSH state blocked without a manual prompt', async () => {
  const { requests, terminal } = await startSSHBackend({ hangDisconnect: true, disconnectStateOnAbort: 'connected', hangStatusAfter: 1, completedLevels:{'0':true} });
  terminal.selection = 'timeout-secret';
  await click(button('Следующий уровень →'));
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 1800)); });
  assert.equal(document.querySelector('[role="status"].terminal-state').textContent, 'ОШИБКА');
  assert.equal(terminal.options.disableStdin, true);
  assert.equal(terminal.output.includes('login as: bandit0'), false);
  assert.ok(button('ОТКЛЮЧИТЬ'), 'unknown session state still exposes the existing disconnect recovery action');
  assert.equal(requests.some(({ path }) => path === '/api/ssh/connect'), false);
  assert.equal(terminal.output.includes('timeout-secret'), false);
});

test('confirmed disconnect retry clears unknown recovery and restores manual input', async () => {
  const { requests, terminal, allowDisconnectRetry } = await startSSHBackend({ hangDisconnect: true, disconnectStateOnAbort: 'connected', hangStatusAfter: 1, completedLevels:{'0':true} });
  await click(button('Следующий уровень →'));
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 1800)); });
  assert.equal(terminal.options.disableStdin, true);
  assert.equal(terminal.output.includes('login as: bandit0'), false);
  allowDisconnectRetry();
  await click(button('ОТКЛЮЧИТЬ'));
  await settle();
  assert.equal(requests.filter(({ path }) => path === '/api/ssh/disconnect').length, 2);
  assert.equal(document.querySelector('[role="status"].terminal-state').textContent, 'НЕ ПОДКЛЮЧЕНО');
  assert.equal(terminal.options.disableStdin, false);
  assert.match(terminal.output, /login as: bandit0/);
});

test('connecting status after disconnect failure blocks input and keeps disconnect retry available', async () => {
  const { requests, terminal } = await startSSHBackend({ disconnectError: true, disconnectStateOnError: 'connecting', statusStateAfter: 'connecting', completedLevels:{'0':true} });
  terminal.selection = 'uncertain-state-secret';
  await click(button('Следующий уровень →'));
  await settle();
  assert.equal(document.querySelector('[role="status"].terminal-state').textContent, 'ПОДКЛЮЧЕНИЕ');
  assert.equal(terminal.options.disableStdin, true);
  assert.equal(terminal.output.includes('login as: bandit0'), false);
  assert.ok(button('ОТКЛЮЧИТЬ'));
  assert.equal(requests.some(({ path }) => path === '/api/ssh/connect'), false);
  assert.equal(terminal.output.includes('uncertain-state-secret'), false);
});

test('unmount cancels an accepted automatic SSH attempt', async () => {
  const { requests, terminal } = await startSSHBackend({completedLevels:{'1-2':true}});
  await moveToLevelOne(terminal);
  terminal.selection = 'unmount-secret';
  await click(button('Следующий уровень →'));
  await settle();
  assert.equal(requests.filter(({ path }) => path === '/api/ssh/connect').length, 1, 'the backend accepted the automatic attempt');
  await act(async () => root.unmount());
  root = null;
  assert.equal(requests.filter(({ path }) => path === '/api/ssh/disconnect').length, 3, 'unmount sends one best-effort cancellation after the two level-transition disconnects');
  assert.equal(terminal.output.includes('unmount-secret'), false);
});

test('side and back navigation cancel a prepared password during session disconnect', async () => {
  for (const target of ['[05]', '← Назад']) {
    localStorage.clear();
    const { requests, terminal, delayNextDisconnect, releaseDisconnect } = await startSSHBackend({completedLevels:{'1-2':true}});
    await moveToLevelOne(terminal);
    delayNextDisconnect();
    terminal.selection = 'must-not-transfer';
    await click(button('Следующий уровень →'));
    await click(button(target));
    terminal.selection = '';
    await act(async () => {
      releaseDisconnect();
      for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
    });
    assert.equal(requests.some(({ path }) => path === '/api/ssh/connect'), false);
    assert.equal(terminal.output.includes('must-not-transfer'), false);
    assert.equal(storageEntries().some(([, value]) => value.includes('must-not-transfer')), false);
    await act(async () => root.unmount());
    root = null;
  }
});

test('CRT reuses its grain buffer and stops rendering when switched off', async () => {
  let allocations = 0, paints = 0, frame, cancelled;
  dom.window.HTMLCanvasElement.prototype.getContext = () => ({
    createImageData: (width, height) => { allocations++; return { data: new Uint8ClampedArray(width * height * 4) }; },
    putImageData: () => { paints++; }
  });
  globalThis.requestAnimationFrame = (fn) => { frame = fn; return 123; };
  globalThis.cancelAnimationFrame = (id) => { cancelled = id; };
  localStorage.setItem('otw_bandit_crt', JSON.stringify('high'));
  await mount();
  frame(50); frame(100);
  assert.equal(paints, 2);
  assert.equal(allocations, 1, 'grain does not allocate a new image every frame');
  await click(button('CRT: HIGH'));
  await click(button('CRT: SOFT'));
  assert.ok(!document.querySelector('.crt-overlay-master'));
  assert.equal(cancelled, 123);
  assert.ok(document.querySelector('.terminal-surface'), 'xterm surface remains available');
});

test('header removes duplicate key, archive and solution actions', async () => {
  await mount();
  const summaries = [...document.querySelectorAll('summary')].map((el) => el.textContent.trim());
  assert.equal(summary('Настройки'), undefined, 'settings are directly available in the header');
  assert.ok(button('SND: ON'));
  assert.ok(button('CRT: SOFT'));
  assert.equal(button('Пароли'), undefined, 'preloaded passwords are absent');
  assert.equal(document.querySelector('input[aria-label="Пользователь SSH"]'), null, 'SSH credentials are entered in xterm');
  assert.equal(document.querySelector('input[aria-label="Пароль SSH"]'), null, 'SSH credentials are entered in xterm');
  assert.equal(button('Подсказка'), undefined);
  assert.ok(button('Решение'), 'the solution has a separate disclosure control');
  assert.ok(button('Справочник команд'), 'the command reference remains available as a disclosure control');
  assert.equal(button('Справочник команд').closest('.mission-support'), button('Решение').closest('.mission-support'));
  assert.equal(button('EXEC // SOLVE'), undefined);
  assert.equal(button('KEY // BANDIT0'), undefined);
  assert.equal(button('АРХИВ [00–33]'), undefined);
  assert.equal(document.querySelectorAll('.app-header-controls > button').length, 2);
  assert.ok(document.querySelector('.header-progress-mode'), 'automatic progress stays a status badge, not a fake toggle');
  assert.ok(document.querySelector('.mission-navigation .btn-invert-secondary'), 'back keeps its Figma button treatment');
  assert.equal(document.querySelectorAll('.terminal-tabs .console-tab').length, 3);
  assert.equal(button('KEYS // [00-33]'), undefined);
  assert.equal(button('JUMP // BANDIT'), undefined);
  assert.equal(button('SYNC'), undefined);
  assert.equal(button('Все'), undefined, 'category chips are replaced by search and groups');
  assert.equal(document.querySelector('button[aria-label="Вставить SSH-команду в терминал"]'), null);
  await click(button('[02]'));
  assert.equal(document.querySelector('.terminal-surface').getAttribute('aria-label'), 'Терминал SSH для bandit1');
  assert.equal(document.querySelectorAll('.terminal-surface').length, 1, 'the interface owns a single live terminal');
});
