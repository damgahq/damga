'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { triggerUrl, sourceView, refusalText, SECRET_NOTE } = require('./assets/source.js');

const base = '/api/v1/tenants/t_home/apps/api/envs/prod';

test('the trigger is written to the app it belongs to', () => {
  assert.strictEqual(triggerUrl(base), `${base}/trigger`);
});

// Three states, and the first one is the one that was invisible for as long as
// nothing wrote a trigger: a push arriving and matching nothing.
test('not connected says so, rather than saying nothing', () => {
  const none = sourceView({ phase: 'unconfigured' });
  assert.strictEqual(none.state, 'unconfigured');
  assert.match(none.text, /reaches nothing/,
    'an unconnected app renders as an empty form, which reads as "there is nothing to do " +' +
    'here" rather than as "a push would be dropped"');

  const connected = sourceView({ phase: 'connected', repoUrl: 'https://x.test/a.git' });
  assert.strictEqual(connected.state, 'connected');
  assert.match(connected.text, /x\.test/);
  assert.strictEqual(new Set([none.text, connected.text,
    sourceView({ phase: 'saving' }).text]).size, 3);
});

// The claim the page must not make.
test('connected does not claim the forge was configured', () => {
  const view = sourceView({ phase: 'connected', repoUrl: 'https://x.test/a.git' });
  assert.match(view.note, /not what has happened|has not seen a push/i,
    'the page says a repository is connected without saying that this is what was ' +
    'configured rather than what has been observed — the platform has never seen the ' +
    "forge's side");
});

test('a refusal is printed as the server phrased it', () => {
  assert.strictEqual(
    refusalText(400, { detail: 'the secret is shorter than 16 characters' }),
    'the secret is shorter than 16 characters');
  assert.match(refusalText(403, null), /owner/);
  assert.match(refusalText(404, null), /nowhere to deploy/);
});

// The rule the endpoint enforces, held on this side too.
test('the page never keeps or shows the secret', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const source = fs.readFileSync(path.join(__dirname, 'assets', 'source.js'), 'utf8');
  for (const forbidden of ['localStorage', 'sessionStorage', 'indexedDB']) {
    assert.ok(!source.includes(forbidden), `source.js uses ${forbidden}`);
  }
  // Cleared after a successful connect, not left in the field.
  assert.match(source, /secret\.value\s*=\s*""/,
    'the secret is left in the field after connecting; a full field is what a screenshot ' +
    'catches');
  // And the note tells the reader it cannot be retrieved.
  assert.match(SECRET_NOTE, /never shown again/);
  assert.match(SECRET_NOTE, /cannot show it to you/,
    'the note does not say the platform itself cannot retrieve it, which is the part ' +
    'that makes "never shown again" a property rather than a policy');
});
