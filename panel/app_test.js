'use strict';

// The panel's first tests.
//
// It had none, and the one place it decides anything decided wrongly for as
// long as the page existed: every deploy that had not finished syncing rendered
// as "was refused", under a banner explaining a refusal that had not happened.
// Nothing could have caught it. The page is 550 lines and this file covers the
// four functions that are not rendering — which is the whole of what it decides.
//
// node --test, no npm, nothing installed. The package's argument against a
// build step holds; it does not extend to having no tests.

const test = require('node:test');
const assert = require('node:assert');

const {
  whatToShow, admissionKnown, short, when, newAppBody, newAppOutcome,
} = require('./assets/app.js');
const { defaultPath } = require('./assets/catalog.js');

const rec = (state, extra = {}) => ({
  seq: 1, state, admission: { allowed: false, reason: '' }, ...extra,
});

test('a running deploy is shown and is not blocked', () => {
  const current = rec('running');
  const got = whatToShow(current, rec('running'));
  assert.strictEqual(got.blocked, false);
  assert.strictEqual(got.shown, current);
});

// The bug, as a test. A deploy on its way is not a deploy that was refused, and
// the page said it was — at the exact moment somebody is most likely to be
// watching it.
test('a deploy still on its way is not reported as refused', () => {
  for (const state of ['pending', 'syncing', 'applied']) {
    const got = whatToShow(null, rec(state));
    assert.strictEqual(got.blocked, false,
      `a ${state} deploy was shown as refused; nothing refused it, it has not ` +
      'finished — and those are different sentences');
  }
});

// And the case the page exists for: when something WAS refused, the reason is
// in the very next record down and the page has to reach for it rather than
// saying "nothing is running" and stopping.
test('a refused deploy is shown so the reason can be', () => {
  const latest = rec('rejected', {
    admission: { allowed: false, reason: 'PolicyViolation', message: 'not signed' },
  });
  const got = whatToShow(null, latest);
  assert.strictEqual(got.blocked, true);
  assert.strictEqual(got.shown, latest,
    'the refused record was not shown, so the reason it was refused is nowhere');
});

test('nothing deployed shows nothing rather than inventing a refusal', () => {
  const got = whatToShow(null, null);
  assert.strictEqual(got.blocked, false);
  assert.strictEqual(got.shown, null);
});

// A running deploy wins over a rejected later one. The rejected record belongs
// to a different attempt; the app is still up, and a page that says otherwise
// is wrong about the thing the reader can see for themselves.
test('a later refusal does not hide what is running', () => {
  const current = rec('running');
  const got = whatToShow(current, rec('rejected'));
  assert.strictEqual(got.blocked, false);
  assert.strictEqual(got.shown, current);
});

// Three answers, not two. With nothing writing admission outcomes, a page that
// can only say admitted or refused has to call every deploy in existence
// refused — which is a claim, made out of a zero value.
test('an unobserved admission is not an admitted or refused one', () => {
  assert.strictEqual(admissionKnown(rec('syncing')), false,
    'a record nothing observed was treated as carrying an outcome');

  assert.strictEqual(admissionKnown(rec('running', {
    admission: { allowed: true, reason: 'Admitted' },
  })), true);

  assert.strictEqual(admissionKnown(rec('rejected', {
    admission: { allowed: false, reason: 'FailedCreate', message: 'denied' },
  })), true, 'a refusal with a reason was treated as unobserved');

  // The state alone is enough. A record moved to rejected was refused whether
  // or not anything filled in the reason.
  assert.strictEqual(admissionKnown(rec('rejected')), true);
});

// Both of these were written against what the functions were assumed to do and
// both were wrong, which is its own small argument for the file existing: short
// truncates past sixteen characters rather than at seven, and when formats a
// date rather than saying "two hours ago". The tests now say what the code does.
test('a long commit is shortened and a short one is left alone', () => {
  // A full SHA-1 is forty characters and gets cut; anything already short
  // enough is not, because an ellipsis on a value that fits is noise.
  assert.strictEqual(short('a07ef31b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f'), 'a07ef31b2c3d…');
  assert.strictEqual(short('a07ef31b2c3d4e5f'), 'a07ef31b2c3d4e5f');

  // The em dash is the point: an absent commit must not render as a real one.
  assert.strictEqual(short(''), '—', 'an absent commit rendered as a value');
  assert.strictEqual(short(null), '—');
  assert.strictEqual(short(undefined), '—');
});

test('a timestamp renders and an absent one does not pretend', () => {
  const stamp = '2026-08-28T09:00:00Z';
  const rendered = when(stamp);
  assert.notStrictEqual(rendered, '—');
  assert.match(rendered, /2026/, 'the date did not survive formatting');

  // A value the browser cannot parse is shown as it arrived rather than as
  // "Invalid Date", which reads like the record is corrupt when the formatter
  // is the thing that failed.
  assert.strictEqual(when('not a date'), 'not a date');

  assert.strictEqual(when(''), '—');
  assert.strictEqual(when(null), '—');
});

// The line the restore rehearsal exists to put on a page, and the three answers
// it has to be able to give.
const { backupView } = require('./assets/app.js');

test('a restored backup says when and how many out of how many', () => {
  const v = backupView({
    database: 'shop-db', state: 'restored',
    finishedAt: '2026-08-29T02:04:11Z', rows: 1284, sourceRows: 1284, tables: 7,
  });
  assert.match(v.lastRun.text, /geri yüklendi/);
  assert.strictEqual(v.lastRun.ok, true);
  // Against the source, not alone. "1,284 came back" and "1,284 came back out
  // of 1,284" are different claims and only the second was measured.
  assert.match(v.verified.text, /1284 satırın 1284 tanesi/);
  assert.match(v.verified.text, /7 tablo/);
});

test('an archive nothing restored is not called restored', () => {
  const v = backupView({
    database: 'shop-db', state: 'backed up', finishedAt: '2026-08-29T02:00:00Z',
  });
  assert.doesNotMatch(v.lastRun.text, /restored/,
    'a backup nobody restored was reported as restored, which is the claim the ' +
    'whole rehearsal exists to make honestly');
  assert.match(v.verified.text, /geri yüklenmedi/);
  assert.strictEqual(v.lastRun.ok, false);
});

test('a database whose first backup has not run says exactly that', () => {
  const v = backupView({ database: 'shop-db', state: 'none yet' });
  assert.match(v.lastRun.text, /henüz hiçbir yedek çalışmadı/);
  assert.strictEqual(v.lastRun.ok, null, 'a run that has not happened was given a verdict');
  // And no count, because there is nothing to count. A zero here reads as a
  // restore that produced nothing.
  assert.strictEqual(v.verified, null);
});

test('one table is a table', () => {
  const v = backupView({
    database: 'db', state: 'restored', finishedAt: '2026-08-29T02:00:00Z',
    rows: 3, sourceRows: 3, tables: 1,
  });
  assert.match(v.verified.text, /1 tablo\b/);
});

// --- adding an application --------------------------------------------------

// The panel could not create one. Measured: its only POST other than signing in
// was /api/v1/login, and the catalogue's form offered the repositories and
// namespaces the tenant already had — so the first app on a new install had to
// be created with curl, and every refusal the endpoint gives, including the one
// that names -git-token-file, was a sentence nobody could reach from the page.

test('the values are trimmed, because the endpoint judges them as typed', () => {
  const got = newAppBody({
    app: '  api ', env: ' prod', repoUrl: ' https://example.test/state.git ',
    branch: ' main ', namespace: ' acme-prod ', path: '',
  });
  assert.deepStrictEqual(got, {
    app: 'api', env: 'prod', repoUrl: 'https://example.test/state.git',
    branch: 'main', path: 'apps/api/prod', namespace: 'acme-prod',
  });
});

// One rule, in two files, that must not drift. Two environments whose manifests
// resolve to one directory are one environment with two names, and the store
// refuses the second with a unique index — so a form that defaulted to
// apps/<app> would walk the second environment into a refusal nobody could
// read. The catalogue learned that; this form has to agree with it.
test('the directory defaults to the same one the catalogue would commit to', () => {
  assert.strictEqual(newAppBody({ app: 'api', env: 'qa' }).path, defaultPath('api', 'qa'));
});

test('a path that was typed is left alone', () => {
  assert.strictEqual(newAppBody({ app: 'api', env: 'prod', path: 'clusters/one' }).path,
    'clusters/one');
});

// The line this whole view exists to put in front of somebody. The server says
// whether anything will apply this app's commits; the panel quotes it and adds
// nothing. Measured on a cluster: without the credential Argo CD reads the
// repository with, the Application is created, the endpoint answers 201, and
// Argo CD sits at "authentication required" where nobody is looking.
test('the delivery line is quoted word for word, including the half that is missing', () => {
  const delivery = "an Argo CD Application is watching this app's directory, but Argo CD was "
    + 'given no credential for this repository, so it can read it only if the repository is '
    + 'public (no git credentials are configured: pass -git-token-file)';
  const got = newAppOutcome(201, { app: { app: 'api', env: 'prod' }, delivery });
  assert.strictEqual(got.ok, true);
  assert.strictEqual(got.note, delivery,
    'the panel summarised the delivery line instead of quoting it; that line is the only '
    + 'place the missing half of the chain is said');
});

test('a created app does not claim anything is running', () => {
  const got = newAppOutcome(201, { app: { app: 'api', env: 'prod' }, delivery: 'watching' });
  assert.match(got.text, /kaydedildi/);
  assert.match(got.hint, /Bu hiçbir şey deploy etmez/);
});

test('a refusal is the server\'s sentence and not the page\'s', () => {
  const got = newAppOutcome(409, { detail: 'repository https://example.test/state.git belongs to another tenant' });
  assert.strictEqual(got.ok, false);
  assert.strictEqual(got.note, 'repository https://example.test/state.git belongs to another tenant');
});

test('a failure with no detail still says what happened', () => {
  const got = newAppOutcome(500, {});
  assert.strictEqual(got.ok, false);
  assert.match(got.text, /HTTP 500/);
});
