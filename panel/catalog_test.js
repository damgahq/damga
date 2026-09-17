'use strict';

// The catalogue page's three decisions.
//
// The page renders what the API says and works nothing out for itself, so there
// are only three functions here that decide anything: what the filter form
// becomes, whether the install button may be offered, and what a refusal says.
// The third is the one that matters — the endpoint names which of three limits
// stopped an install, and a page that replaces that with "cannot be installed"
// throws away the only part a person can act on.

const test = require('node:test');
const assert = require('node:assert');

const { catalogQuery, canInstall, refusals, planError } = require('./assets/catalog.js');

test('an empty filter asks for the whole catalogue', () => {
  assert.strictEqual(catalogQuery(), '');
  assert.strictEqual(catalogQuery({ text: '   ' }), '',
    'whitespace became a search for whitespace, which matches nothing');
});

// The parameter names are a contract with the handler, and the handler ignores
// what it does not recognise: a name spelled wrong here answers the whole
// catalogue, which reads as a search box that does not work.
test('the filter uses the names the endpoint reads', () => {
  const query = catalogQuery({ text: 'n8n', category: 'automation' });
  assert.match(query, /(^|[?&])q=n8n(&|$)/, `the search text is not in ${query}`);
  assert.match(query, /(^|[?&])category=automation(&|$)/, `the category is not in ${query}`);
});

// Two tags narrow. One parameter carrying "a,b" is a single tag nothing has,
// and the list would come back empty with no way to tell why.
test('two tags are two parameters and not one joined string', () => {
  const query = catalogQuery({ tags: ['self-hosted', 'analytics'] });
  const tags = new URLSearchParams(query.slice(1)).getAll('tag');
  assert.deepStrictEqual(tags, ['self-hosted', 'analytics'],
    `two tags arrived as ${JSON.stringify(tags)}`);
});

// The button is a promise that pressing it will work. Anything other than the
// API saying so is the page not knowing.
test('the install button is offered only when the API said installable', () => {
  assert.strictEqual(canInstall({ installable: true }), true);
  for (const plan of [null, undefined, {}, { installable: false },
    { detail: 'this installation has no catalogue', status: 503 }]) {
    assert.strictEqual(canInstall(plan), false,
      `${JSON.stringify(plan)} offered an install button for something nothing said would work`);
  }
});

test('a refusal is shown in the words the endpoint used', () => {
  const plan = {
    installable: false,
    refusals: ['this template asks the platform to invent 1 value(s) — SERVICE_PASSWORD_N8N'],
  };
  assert.deepStrictEqual(refusals(plan), plan.refusals,
    'the reason was rewritten; the endpoint names which of three limits stopped it and that ' +
    'is the only part a person can act on');
});

test('no reason from the API is no reason invented', () => {
  assert.deepStrictEqual(refusals({ installable: false }), []);
  assert.deepStrictEqual(refusals(null), []);
});

// --- the install form -------------------------------------------------------

const {
  installTargets, defaultPath, installBody, installOutcome,
} = require('./assets/catalog.js');

// The three values this page used to invent, and the reason inventing them was
// worse than leaving them blank.
test('the form offers what the tenant already has and invents nothing', () => {
  const targets = installTargets([
    { app: 'api', env: 'prod', repoUrl: 'https://git/acme', branch: 'main', namespace: 'damga' },
    { app: 'web', env: 'prod', repoUrl: 'https://git/acme', branch: 'main', namespace: 'damga' },
    { app: 'api', env: 'dev', repoUrl: 'https://git/acme-dev', branch: 'work', namespace: 'damga' },
  ]);
  assert.deepStrictEqual(targets.repos, ['https://git/acme', 'https://git/acme-dev']);
  assert.deepStrictEqual(targets.namespaces, ['damga'], 'the same namespace twice is one option');
  assert.deepStrictEqual(targets.branches, ['main', 'work']);
});

// A tenant with nothing deployed has nothing to offer, and that is the case the
// page must not paper over: nothing in this platform creates a namespace, so a
// name it made up would be committed and then refused by the cluster.
test('a tenant with nothing deployed yields no options rather than a guess', () => {
  const empty = installTargets([]);
  assert.deepStrictEqual(empty, { repos: [], namespaces: [], branches: [] });
  assert.deepStrictEqual(installTargets(undefined).namespaces, []);
});

// Two environments that resolve to one file: the platform refuses the second,
// and a page that offered `apps/${app}` would walk every user into it.
test('the environment is in the path, so two of them are two places', () => {
  assert.strictEqual(defaultPath('api', 'prod'), 'apps/api/prod');
  assert.notStrictEqual(defaultPath('api', 'prod'), defaultPath('api', 'dev'));
});

test('the dry run and the install send the same body', () => {
  const entry = { name: 'gotify' };
  const form = { app: 'notes', env: 'staging', repoUrl: 'https://git/acme', branch: 'main', namespace: 'damga' };

  const preview = installBody(entry, form, { dryRun: true });
  const real = installBody(entry, form);
  assert.strictEqual(preview.dryRun, true);
  assert.strictEqual(real.dryRun, undefined);

  const { dryRun, ...previewRest } = preview;
  assert.deepStrictEqual(previewRest, real,
    'a preview of a different request is not a preview');
  assert.strictEqual(real.path, 'apps/notes/staging', 'the path follows the name and the environment');
  assert.strictEqual(real.namespace, 'damga');
  assert.strictEqual(real.repoUrl, 'https://git/acme');
});

test('an explicit path is kept and whitespace is not sent', () => {
  const body = installBody({ name: 'g' }, { app: ' notes ', env: ' prod ', path: ' apps/x ' });
  assert.strictEqual(body.path, 'apps/x');
  assert.strictEqual(installBody({ name: 'g' }, { app: 'notes', env: 'prod' }).path, 'apps/notes/prod');
});

// The one that matters. 201 means the placement was written and the manifest
// was committed; it does not mean anything is running, and the two are minutes
// and a whole subsystem apart.
test('a successful install says committed, and says nothing is running yet', () => {
  const out = installOutcome(201, {
    app: { app: 'notes', env: 'prod' },
    deploy: { seq: 4, state: 'pending' },
  });
  assert.strictEqual(out.ok, true);
  assert.match(out.text, /commit edildi/);
  assert.doesNotMatch(out.text, /running/,
    'the headline must not claim the thing that has not happened');
  assert.match(out.note, /pending/, 'the deploy state is the API\'s answer and is quoted');
  assert.match(out.note, /çalışan bir şey olmaz/);
  assert.match(out.note, /notes/, 'the reader is told where to watch it');
});

test('a refused install lists every reason the endpoint gave', () => {
  const out = installOutcome(422, {
    installable: false,
    refusals: ['this template becomes 2 objects', 'NOTES_KEY asks for a "user" value'],
  });
  assert.strictEqual(out.ok, false);
  assert.strictEqual(out.reasons.length, 2,
    'one reason at a time sends the reader round the loop by the endpoint\'s choice');
});

test('a name already taken is not reported as a broken install', () => {
  const out = installOutcome(409, { detail: 'this app and environment already exist' });
  assert.strictEqual(out.ok, false);
  assert.match(out.text, /zaten/);
  assert.strictEqual(out.note, 'this app and environment already exist');
});

// Silence is the failure this whole task was about. Every other status has to
// produce a sentence, and it has to be the server's where there is one.
test('no status ends in silence', () => {
  for (const [status, body] of [
    [501, { detail: 'this installation cannot start builds' }],
    [503, { detail: 'this installation has no catalogue' }],
    [500, null],
    [400, { detail: 'an install needs a repository to write its manifests to' }],
  ]) {
    const out = installOutcome(status, body);
    assert.strictEqual(out.ok, false);
    assert.ok(out.text && out.text.length > 0, `HTTP ${status} produced no sentence`);
    if (body && body.detail) {
      assert.strictEqual(out.text, body.detail, 'the server said it better than this page can');
    }
  }
});

// A refusal the panel could not show, because it never looked at the status.
//
// catalogApi returns { status, body } rather than throwing, and select() used
// to hand the body straight to render(). A 400 has no workloads and no
// refusals, so render() drew the entry's name and stopped: clicking an entry
// looked like clicking nothing. It happened on exactly the tenant most likely
// to be looking — a new one, whose form defaults are empty because they come
// from apps it does not have yet.
test('a failed plan says what the server said, not nothing', () => {
  assert.equal(
    planError(400, { detail: 'an install needs an app name, an environment and a namespace', status: 400 }),
    'an install needs an app name, an environment and a namespace',
  );
});

test('a failed plan with no detail still names the status', () => {
  const said = planError(502, null);
  assert.match(said, /502/, 'the status is the only thing left to say and it has to be said');
});

// The form has to be built before the plan can send render() home.
//
// Structural, and it is the only shape available: render() assembles DOM and is
// not exported, so this reads the file the way wiring_test.js does. It is bound
// here because the defect was an ordering — the form, the namespace note and
// the repository note all sat after an early return on canInstall(), so a
// tenant with nothing deployed got a refusal it had no way to answer, and the
// note written for exactly that tenant was invisible to exactly that tenant.
test('the install form is built before the plan can return early', () => {
  const src = require('node:fs').readFileSync(require.resolve('./assets/catalog.js'), 'utf8');
  const form = src.indexOf("catalogEl('div', { class: 'install-form' }");
  const gate = src.indexOf('if (!canInstall(plan))');
  assert.ok(form !== -1, 'the install form is gone');
  assert.ok(gate !== -1, 'the plan no longer gates anything');
  assert.ok(
    form < gate,
    'canInstall() returns before the form is built, so a refused plan hides the fields that would fix it',
  );
});
