'use strict';

// The settings view's decisions, which are the parts that are not rendering.
//
// node --test, no npm. CI runs `node --test panel/*_test.js`, so this file
// runs by existing.

const test = require('node:test');
const assert = require('node:assert');

const {
  settingsUrl, normalise, envRowView, scopeText, scopeIsUseless,
  settingsBody, refusalText, savedText, warningsOf,
} = require('./assets/settings.js');

const base = '/api/v1/tenants/t_home/apps/api/envs/prod';

test('the settings are read from the app they belong to', () => {
  assert.strictEqual(settingsUrl(base), `${base}/settings`);
});

// Three screens over the two states the wire actually has.
//
// The endpoint sends a literal value (possibly "") or a secret with no value.
// There is no third wire state: the CRD's EnvVar is {name, value}, so "absent"
// and "" are the same object. An earlier draft of this file drew a fourth
// state for "declared with no value" and would have shown a difference the
// data cannot carry — the mirror of the mistake metrics.js exists to prevent,
// and just as wrong.
//
// The screen still has three, because "" draws as an empty box and an empty
// box reads as nothing at all. "" is a real setting and says so in words.
test('a value, an empty value and a secret are three screens', () => {
  const states = [
    envRowView({ value: 'postgres://…' }),
    envRowView({ value: '' }),
    envRowView({ secret: true }),
  ];
  assert.deepStrictEqual(states.map((s) => s.state), ['plain', 'empty', 'secret']);
  assert.strictEqual(new Set(states.map((s) => s.text)).size, 3,
    'two of the three say the same sentence, so two different facts render identically');
  assert.notStrictEqual(states[1].text, '',
    'the empty string renders as an empty box, which reads as no value at all rather than ' +
    'as a variable the application sees as present and empty');
});

// The secret's value is never on this page.
test('a secret is never rendered, and says so rather than looking unset', () => {
  const view = envRowView({ secret: true });
  assert.strictEqual(view.shown, false,
    'the panel would render a secret value, which makes this page the one place it is readable');
  assert.match(view.text, /gösterilmiyor/);
  assert.notStrictEqual(view.text, envRowView({}).text,
    'a secret reads the same as a variable with no value; one is set and one is not');
});

// The 2026-09-04 decision, on the screen, in the endpoint's words.
//
// It was written down with the 2026-09-02 drift measurement behind it, and the
// user asked for it where somebody sets a secret rather than in a document.
// The endpoint sends the sentence; this page prints it rather than keeping a
// second copy that can drift from the first.
test('a secret carries the server sentence about what it costs', () => {
  const note = 'This value is not in git. Git carries the name and which Secret it lives in; ' +
    'the value itself is written straight into the cluster, so there is no "who changed it ' +
    'and when" for it, no rollback, and if the Secret is deleted nothing puts it back — ' +
    'unlike a manifest, which Argo CD restores in about ten seconds.';
  const view = envRowView({ secret: true }, note);
  assert.strictEqual(view.warning, note,
    'the panel rewrote the endpoint sentence instead of printing it');
  assert.strictEqual(view.inGit, false);

  // And when the endpoint sent none — an older control plane — it still says
  // the two things that matter rather than nothing.
  const fallback = envRowView({ secret: true });
  assert.match(fallback.warning, /git'te değil/i);
  assert.match(fallback.warning, /geri alınamaz/i,
    'the fallback does not say the value cannot be rolled back');
  assert.match(fallback.warning, /silinirse|geri koymaz/i,
    'the fallback does not say that nothing restores it if the Secret is deleted');

  // No literal carries it, or the sentence stops meaning anything.
  for (const plain of [envRowView({ value: 'x' }, note), envRowView({ value: '' }, note)]) {
    assert.strictEqual(plain.warning, '');
    assert.strictEqual(plain.inGit, true);
  }
});

// The one that would destroy something unrecoverable.
test('saving without retyping a secret does not clear it', () => {
  const body = settingsBody([
    { key: 'DB_PASSWORD', secret: true },
    { key: 'PORT', value: '3000', runtime: true },
  ]);
  const secret = body.env.find((e) => e.key === 'DB_PASSWORD');
  assert.ok(!Object.prototype.hasOwnProperty.call(secret, 'value'),
    'the save sends a value for a secret nobody retyped. Sending "" clears it, and the ' +
    'value is not in git — so a save that only changed a health check would destroy a ' +
    'password with nothing to restore it from');
  // The ordinary field still goes.
  assert.strictEqual(body.env.find((e) => e.key === 'PORT').value, '3000');
});

test('a secret that was retyped is sent', () => {
  const typed = settingsBody([{ key: 'S', secret: true, typed: true, value: 'new' }]);
  assert.strictEqual(typed.env[0].value, 'new');
});

// Deletion is omission, and there is no clear verb.
//
// The endpoint answers 400 to a secret carrying "" — "a secret with an empty
// value is not a secret" — so the panel has no shape that can express a clear
// by accident. Removing a variable means it is not in the list the save sends.
test('a removed variable is simply not in the body', () => {
  const kept = settingsBody([{ key: 'A', value: '1' }, { key: 'B', value: '2' }]);
  assert.deepStrictEqual(kept.env.map((e) => e.key), ['A', 'B']);

  const removed = settingsBody([{ key: 'A', value: '1' }]);
  assert.deepStrictEqual(removed.env.map((e) => e.key), ['A'],
    'the save carries a variable the reader removed, so a full-replace PUT puts it back');
});

// Two booleans, not one choice.
test('build and runtime are independent, and applying nowhere is named', () => {
  assert.strictEqual(scopeText({ build: true, runtime: true }), 'derleme ve çalışma zamanı');
  assert.strictEqual(scopeText({ build: true, runtime: false }), 'yalnızca derleme');
  assert.strictEqual(scopeText({ build: false, runtime: true }), 'yalnızca çalışma zamanı');
  assert.match(scopeText({ build: false, runtime: false }), /hiçbir yere uygulanmıyor/,
    'a variable that applies at neither build nor runtime is drawn as though it were fine; ' +
    'it is set and does nothing');

  assert.strictEqual(scopeIsUseless({ build: false, runtime: false }), true);
  for (const entry of [{ build: true }, { runtime: true }, { build: true, runtime: true }]) {
    assert.strictEqual(scopeIsUseless(entry), false);
  }

  // The four combinations are four sentences.
  const all = [[0, 0], [0, 1], [1, 0], [1, 1]]
    .map(([b, r]) => scopeText({ build: Boolean(b), runtime: Boolean(r) }));
  assert.strictEqual(new Set(all).size, 4);
});

// The server's words, and there is one shape.
//
// The catalogue answers with a list because three limits can stop an install
// at once; this endpoint answers with the first bad field, so a page drawing a
// list would be drawing an array of one for ever.
test('a refusal is printed as the server phrased it', () => {
  assert.strictEqual(refusalText(400, { detail: 'a secret with an empty value is not a secret' }),
    'a secret with an empty value is not a secret');
  assert.strictEqual(refusalText(400, { detail: 'PORT is set both as a literal and as a secret' }),
    'PORT is set both as a literal and as a secret');
  assert.match(refusalText(403, null), /ayarları değiştirme izni yok/);
  assert.match(refusalText(499, null), /499/,
    'an unexpected status is reported with its number rather than as a generic failure');
});

// A settings change is a commit — and sometimes it is not, and those are two
// different sentences.
//
// A PUT that changed only secret VALUES answers 200 with a null record,
// because git never carries those values and nothing was written to it. The
// change really happened, so "saved" alone is useless and "committed" is a
// lie. It has to say which.
test('a save says whether anything was committed', () => {
  assert.match(savedText({ record: { commit: 'abcdef1234567' } }), /abcdef1 olarak commit edildi/);

  const secretsOnly = savedText({ record: null });
  assert.match(secretsOnly, /hiçbir şey commit edilmedi/,
    'a save that wrote only secret values claims a commit that does not exist');
  assert.match(secretsOnly, /git/,
    'it does not say why there is no commit, which is the one thing that makes this ' +
    'normal rather than a failure');
  assert.notStrictEqual(secretsOnly, savedText({ record: { commit: 'abc1234' } }));
});

// The endpoint's warnings, printed rather than inferred.
//
// It knows things this page cannot: that BuildSpec has no Env field today, so
// a build-time variable is recorded and consumed by nothing. A setting that is
// stored and does nothing is exactly the failure this page exists to stop
// being invisible.
test('the server warnings are carried through', () => {
  assert.deepStrictEqual(warningsOf({ warnings: ['BUILD_ARG is build-time and nothing reads it'] }),
    ['BUILD_ARG is build-time and nothing reads it']);
  assert.deepStrictEqual(warningsOf({}), []);
  assert.deepStrictEqual(warningsOf(null), []);
});

// The wire shape lives in one function.
test('an absent literal value is the empty string, and a secret has none', () => {
  const { env } = normalise({ env: [{ key: 'A' }, { key: 'B', value: '' }, { key: 'C', secret: true }] });
  assert.strictEqual(env[0].value, '',
    'an absent literal value became undefined, which invents a "no value" state the CRD ' +
    'cannot represent');
  assert.strictEqual(env[1].value, '');
  assert.strictEqual(env[2].value, undefined,
    'a secret came back carrying a value; the endpoint does not send one and the panel ' +
    'must not manufacture one');
  assert.strictEqual(env[2].secret, true);
  assert.deepStrictEqual(normalise(null).env, []);
  assert.deepStrictEqual(normalise({}).env, []);
});

test('the Secret a variable reads is carried through, because git shows it', () => {
  const { env } = normalise({
    env: [{ key: 'DB', secret: true, secretRef: { name: 'api-env', key: 'DB' } }],
  });
  assert.deepStrictEqual(env[0].secretRef, { name: 'api-env', key: 'DB' },
    'the secretRef was dropped: which Secret a variable reads is the thing git shows and ' +
    'Coolify does not show at all, and it is the point of the 2026-09-04 decision');
});

// The rule this page inherits from exec.js and from the server's log.
test('nothing typed on this page is stored by the browser', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const source = fs.readFileSync(path.join(__dirname, 'assets', 'settings.js'), 'utf8');
  for (const forbidden of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie']) {
    assert.ok(!source.includes(forbidden),
      `settings.js uses ${forbidden}: this page is where passwords are typed, and the ` +
      'server refuses to log a command argument for exactly this reason');
  }
  assert.ok(!/console\.(log|info|warn|error)\s*\(/.test(source),
    'settings.js logs to the console: a value typed here would land in the browser log');
  const autocompletes = source.match(/autocomplete["'],\s*["']off/g) || [];
  assert.ok(autocompletes.length >= 2,
    'the name and value fields are not both autocomplete=off, so the browser keeps its own ' +
    'copy of every secret typed into them');
});
