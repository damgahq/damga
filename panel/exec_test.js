'use strict';

// The exec view's decisions, which are the parts of it that are not rendering.
//
// Its own file, the same way logs_test.js is: node --test, no npm, nothing
// installed. CI runs `node --test panel/*_test.js`, so adding this file is
// enough to have it run.

const test = require('node:test');
const assert = require('node:assert');

const {
  execUrl, execView, refusalText, parseCommand, parseFrames, appendOutput, MAX_OUTPUT,
} = require('./assets/exec.js');

const base = '/api/v1/tenants/t_home/apps/api/envs/prod';

test('the command is POSTed to the app it is about', () => {
  assert.strictEqual(execUrl(base), `${base}/exec`);
});

// The rule metrics.js set, applied to five sentences instead of three.
//
// The server tells five refusals apart and says which pods it saw and what
// each was doing. If the page folds them into one box, the work that keeps
// CrashLoopBackOff apart from ImagePullBackOff was done for a screen nobody
// reads.
test('each refusal the server names arrives on screen in its own words', () => {
  const refusals = [
    'api has no pods in t-acme: nothing is deployed, or it is scaled to zero',
    'no pod is running: api-0 (CrashLoopBackOff), api-1 (ImagePullBackOff)',
    'no pod is running: api-0 (running but not ready)',
    'no pod is running: api-0 (terminating)',
    'this pod has more than one container (app, log-shipper); say which with {"container": "..."}',
    'no such container "web" in pod api-0; it has app, log-shipper',
    'no command was given: send {"command": [...]} with at least one element',
  ];
  for (const detail of refusals) {
    const view = execView({ phase: 'refused', detail });
    assert.strictEqual(view.text, detail,
      'the panel rewrote the refusal instead of printing it; the reason now lives in two ' +
      'vocabularies that have to be kept in step');
    assert.strictEqual(view.state, 'refused');
    assert.strictEqual(view.ran, false,
      'a refused command is drawn as though something ran');
  }
});

// The distinction the API makes, kept by the page.
//
// One of the eight revert directions on the endpoint was "a non-zero exit
// reported as a failure". Making that distinction on the server and losing it
// here would leave the reader with the same wrong screen.
test('a command that exits non-zero ran, and does not read as a refusal', () => {
  const one = execView({ phase: 'exited', code: 1 });
  assert.strictEqual(one.state, 'exited',
    'a command that exited 1 is drawn as a refusal; it ran, and it printed why');
  assert.strictEqual(one.ran, true);
  assert.match(one.text, /1 ile çıktı/);
  assert.doesNotMatch(one.text, /could not|failed to run|unable/i,
    'the text says the command could not be run, and it was: it ran and returned 1');

  const zero = execView({ phase: 'exited', code: 0 });
  assert.strictEqual(zero.ok, true);
  assert.match(zero.text, /0 ile çıktı/);

  // The two states are different states, not one state with a different colour.
  assert.notStrictEqual(one.state, execView({ phase: 'refused', detail: 'x' }).state);
});

// The expensive silence.
//
// A pane that stops filling is exactly what a command printing nothing looks
// like. The difference is that this one may have finished and changed
// something, and the reader cannot see what.
test('a broken stream never reads as a command that printed nothing', () => {
  const lost = execView({ phase: 'lost' });
  assert.strictEqual(lost.state, 'lost');
  assert.strictEqual(lost.ok, false);
  assert.strictEqual(lost.ran, true,
    'a lost stream is drawn as though nothing ran; the request was accepted, so ' +
    'something did');
  assert.match(lost.text, /bağlantı koptu/i);
  assert.match(lost.text, /Bitmiş olabilir|çıktısı burada kesiliyor/i,
    'the message does not say the command may have run, which is the only thing the ' +
    'reader needs from this state');

  // And it is not the idle screen either, which is the other thing an empty
  // pane could honestly mean.
  assert.notStrictEqual(lost.state, execView({ phase: 'idle' }).state);
  assert.strictEqual(execView({ phase: 'idle' }).ran, false);
});

test('every state this view can be in is a different screen', () => {
  // Each state given what it actually receives. Feeding one shared detail to
  // all of them made 'refused' and 'lost' echo the same string and this case
  // failed for a reason that was not about the code.
  const states = [
    execView({ phase: 'idle' }),
    execView({ phase: 'running' }),
    execView({ phase: 'exited', code: 0 }),
    execView({ phase: 'refused', detail: 'no pod is running: api-0 (CrashLoopBackOff)' }),
    execView({ phase: 'lost' }),
  ];
  const names = new Set(states.map((s) => s.state));
  assert.strictEqual(names.size, 5,
    'two of the five states render as the same thing, which is the folding this file exists ' +
    'to prevent');
  const texts = new Set(states.map((s) => s.text));
  assert.strictEqual(texts.size, 5, 'two states say the same sentence');
});

// A refusal that arrives as a status rather than an event.
test('a refusal before the stream prints the detail the server sent', () => {
  assert.strictEqual(
    refusalText(403, { detail: 'no access to this tenant' }), 'no access to this tenant');
  assert.match(refusalText(501, {}), /komut çalıştıramıyor/);
  assert.match(refusalText(403, {}), /yalnızca sahibe/);
  assert.match(refusalText(418, null), /418/,
    'an unexpected status is reported with its number rather than as a generic failure');
});

test('what somebody typed becomes the argv the endpoint takes', () => {
  assert.deepStrictEqual(parseCommand('rails db:migrate'), ['rails', 'db:migrate']);
  assert.deepStrictEqual(parseCommand('psql -c "select 1"'), ['psql', '-c', 'select 1']);
  assert.deepStrictEqual(parseCommand("sh -c 'echo a b'"), ['sh', '-c', 'echo a b']);
  assert.deepStrictEqual(parseCommand('  spaced   out  '), ['spaced', 'out']);
  // An empty quoted argument is an argument, and dropping it shifts every
  // argument after it one place left.
  assert.deepStrictEqual(parseCommand('psql -c ""'), ['psql', '-c', '']);
  assert.deepStrictEqual(parseCommand(''), []);

  assert.throws(() => parseCommand('psql -c "select 1'), /kapatılmamış bir " tırnağı/,
    'an unbalanced quote was closed silently, which runs a command nobody wrote');
});

// The input is never wrapped in a shell.
test('the command is sent as typed, and not handed to sh -c', () => {
  const argv = parseCommand('rails db:migrate');
  assert.strictEqual(argv[0], 'rails',
    'the command is wrapped in a shell, so it now needs the image to have one — and a ' +
    'distroless image does not');
  assert.ok(!argv.includes('-c'));
});

test('the server heartbeat does not reach the screen as output', () => {
  const { events } = parseFrames(': beat\n\nevent: stdout\ndata: {"text":"hi"}\n\n');
  assert.strictEqual(events.length, 1,
    'the heartbeat is rendered as output, so a slow silent command looks like a chatty one');
  assert.deepStrictEqual(events[0], { event: 'stdout', data: { text: 'hi' } });
});

test('a frame split across two chunks is not lost or halved', () => {
  const first = parseFrames('event: stdout\ndata: {"text":"hel');
  assert.strictEqual(first.events.length, 0);
  const second = parseFrames(first.rest + 'lo"}\n\n');
  assert.deepStrictEqual(second.events[0].data, { text: 'hello' });
});

test('a frame this file cannot parse is shown rather than dropped', () => {
  const { events } = parseFrames('event: stdout\ndata: not json\n\n');
  assert.strictEqual(events.length, 1,
    'unreadable output was dropped, which takes text off the screen and tells the reader ' +
    'the command printed less than it did');
  assert.match(events[0].data.text, /not json/);
});

test('output has a ceiling, and keeps the newest', () => {
  const kept = appendOutput('a'.repeat(MAX_OUTPUT), 'TAIL', MAX_OUTPUT);
  assert.strictEqual(kept.length, MAX_OUTPUT);
  assert.ok(kept.endsWith('TAIL'), 'the ceiling drops the end somebody is watching');
});

// The rule the server applies to its log, applied to the browser.
//
// The endpoint records the program and never the arguments, because a command
// line is where people put a password. A field the browser remembers would
// undo that decision on the reader's own machine, where the tenancy that
// protects the server's log does not reach.
test('nothing about the typed command is stored anywhere', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const source = fs.readFileSync(path.join(__dirname, 'assets', 'exec.js'), 'utf8');
  for (const forbidden of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie']) {
    assert.ok(!source.includes(forbidden),
      `exec.js uses ${forbidden}: the server refuses to log the arguments because a command ` +
      'line is where a password ends up, and this would keep them on the reader\'s disk');
  }
  assert.match(source, /autocomplete["']?,\s*["']off/,
    'the command field is not autocomplete=off, so the browser keeps its own history of ' +
    'every argument typed into it');
});

// The one thing this view must never do on its own.
test('a broken connection is not retried, because the command already ran', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const source = fs.readFileSync(path.join(__dirname, 'assets', 'exec.js'), 'utf8');
  assert.ok(!/setTimeout\s*\(\s*(open|run|send)/.test(source),
    'exec.js reschedules the request: logs.js retries because reopening a log stream costs ' +
    'nothing, and re-running a migration is the whole harm this endpoint can do');
  assert.ok(!source.includes('new EventSource'),
    'exec.js uses EventSource, which reconnects on its own and would re-run the command');
});
