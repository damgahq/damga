'use strict';

// The database view's decisions. node --test, no npm; CI runs
// `node --test panel/*_test.js`, so this file runs by existing.

const test = require('node:test');
const assert = require('node:assert');

const {
  databasesUrl, credentialsView, claimsView, claimsWanted, backupsOffered,
  refusalText, removedText, CREDENTIALS_NOTE,
} = require('./assets/databases.js');

const base = '/api/v1/tenants/t_home/apps/api/envs/prod';

test('databases are read from the app they belong to', () => {
  assert.strictEqual(databasesUrl(base), `${base}/databases`);
});

// The settings page's secret row, wearing a different hat.
//
// A database password is in exactly the class the 2026-09-04 decision was
// about: minted into a Secret the application reads, with git carrying the
// name and never the value. Same two consequences, so the same sentence.
test('the credentials say where they are and what that costs', () => {
  const view = credentialsView({ secretName: 'main' });
  assert.strictEqual(view.shown, false,
    'the panel would render a database password');
  assert.strictEqual(view.inGit, false);
  assert.match(view.text, /main Secret/);
  assert.match(view.warning, /git'te değil/i);
  assert.match(view.warning, /geri dönüş yoktur/i,
    'the warning does not say there is no rollback to a value git never held');
  assert.match(view.warning, /silinirse|geri koymaz/i,
    'the warning does not say that nothing restores the Secret if it is deleted — the ' +
    'drift case measured on 2026-09-02');

  // Even with nothing to name, it must not read as "there is no password".
  const bare = credentialsView({});
  assert.match(bare.text, /bu sayfanın okumadığı/);
  assert.strictEqual(bare.warning, CREDENTIALS_NOTE);
});

// Three things to be told about the budget, and the middle one is the one
// worth seeing before the button is pressed.
test('the volume budget is three screens, not a number', () => {
  const room = claimsView({ budget: 4, usedByTheseHere: 1 }, 1);
  const last = claimsView({ budget: 4, usedByTheseHere: 3 }, 1);
  const over = claimsView({ budget: 4, usedByTheseHere: 3 }, 2);

  assert.deepStrictEqual([room.state, last.state, over.state], ['room', 'last', 'over']);
  assert.strictEqual(new Set([room.text, last.text, over.text]).size, 3,
    'two of the three say the same sentence');
  assert.match(last.text, /sonuncusunu/,
    'taking the final claim reads the same as taking any other, so nobody is warned ' +
    'before the one that fills the namespace');

  // The refusal is honest about when it arrives: not now.
  assert.match(over.text, /Argo CD|syncs/,
    'the over-budget message implies the request will be refused now. The write path ' +
    'is git: the commit is made and the cluster refuses it later');
  assert.strictEqual(over.ok, false);

  // No budget in the response is not "zero left".
  assert.strictEqual(claimsView(null, 1).state, 'unknown');
  assert.strictEqual(claimsView({}, 1).text, '',
    'an unknown budget renders a sentence about a number nobody sent');
});

// Backups are two claims, and the page says so before the choice.
test('asking for backups doubles what the database costs', () => {
  assert.strictEqual(claimsWanted({ engine: 'postgres', backups: false }), 1);
  assert.strictEqual(claimsWanted({ engine: 'postgres', backups: true }), 2,
    'a backed-up database is drawn as costing one claim; the archives get a volume of ' +
    'their own so that filling it cannot stop the database accepting writes');
  // Redis is refused backups by the API, so it never costs two.
  assert.strictEqual(claimsWanted({ engine: 'redis', backups: true }), 1);
});

test('redis is not offered backups rather than offered them and refused', () => {
  assert.strictEqual(backupsOffered('postgres'), true);
  assert.strictEqual(backupsOffered('redis'), false,
    'the form offers redis a backup schedule the API refuses: it would run pg_dump ' +
    'against a server that has never heard of it');
});

test('a refusal is printed as the server phrased it', () => {
  assert.strictEqual(
    refusalText(400, { detail: 'the image "postgres" carries no tag or digest' }),
    'the image "postgres" carries no tag or digest');
  assert.match(refusalText(403, null), /yalnızca sahibe/);
  assert.match(refusalText(418, null), /418/);
});

// The sentence that stops "removed" meaning "the data is gone".
test('a removal prints the server sentence about the data', () => {
  const note = 'the database is withdrawn: its StatefulSet, Service and credentials ' +
    'Secret are owned by it and go with it. The data volume and the backup volume do not';
  assert.strictEqual(removedText({ note }), note,
    'the panel rewrote the endpoint sentence instead of printing it');

  // And with no note, it still does not say the data is gone.
  const fallback = removedText({});
  assert.match(fallback, /silinmez/,
    'a removal with no note from the server reads as though the data went with it');
});

test('nothing about a database is stored by the browser', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const source = fs.readFileSync(path.join(__dirname, 'assets', 'databases.js'), 'utf8');
  for (const forbidden of ['localStorage', 'sessionStorage', 'indexedDB']) {
    assert.ok(!source.includes(forbidden), `databases.js uses ${forbidden}`);
  }
  // And nothing in the view reads or sends a password field.
  //
  // Matched as a field and not as a word. The first version of this forbade
  // "password" outside comments and failed on CREDENTIALS_NOTE — the sentence
  // whose whole job is to tell the reader there is no password here. A guard
  // that cannot tell a field from prose about the absence of one makes the
  // prose unwritable, which is the same shape that caught the exec guard on
  // install.sh's own explanation.
  for (const shape of [/\.password\b/, /\bpassword\s*:/, /["']password["']\s*[:\]]/]) {
    assert.ok(!shape.test(source),
      `databases.js touches a password field (${shape}): the endpoint has no field for ` +
      'one, and this page must have nowhere to put one');
  }
});
