'use strict';

// That the page actually loads the files it is made of.
//
// This exists because it did not, and nothing noticed. logs.js and catalog.js
// were both written, both tested, and neither was ever loaded by index.html or
// referenced by app.js — a log view and a catalogue that passed their tests and
// could not be reached in a browser. Their unit tests went on passing the whole
// time, because a module's tests require it directly and never ask whether the
// page does.
//
// The panel has no build step, which is what makes this checkable with a string
// search and also what makes it necessary: nothing resolves an import, so a
// missing script tag is a missing global at runtime and silence at every other
// time.

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const assets = path.join(__dirname, 'assets');
const read = (name) => fs.readFileSync(path.join(assets, name), 'utf8');

// Each view: the file, the global it publishes, and the word app.js reaches it
// by. Adding a view means adding a row, which is the point — the next one
// cannot be orphaned by being forgotten here, because forgetting it here is
// the same act as forgetting it in the page.
const views = [
  { file: 'metrics.js', global: 'damgaMetrics' },
  { file: 'logs.js', global: 'damgaLogs' },
  { file: 'catalog.js', global: 'damgaCatalog' },
  { file: 'exec.js', global: 'damgaExec' },
  { file: 'settings.js', global: 'damgaSettings' },
  { file: 'databases.js', global: 'damgaDatabases' },
  { file: 'source.js', global: 'damgaSource' },
];

test('every view is loaded by the page, before the script that uses it', () => {
  const html = read('index.html');
  const appAt = html.indexOf('/app.js');
  assert.ok(appAt > -1, 'index.html does not load app.js at all');

  for (const view of views) {
    const at = html.indexOf(`/${view.file}`);
    assert.ok(at > -1,
      `index.html never loads ${view.file}: it is written, it is tested, and in a ` +
      'browser it does not exist');
    assert.ok(at < appAt,
      `${view.file} is loaded after app.js, so the global it publishes is not there ` +
      'when app.js looks for it');
  }
});

test('every view publishes the global the page looks for', () => {
  for (const view of views) {
    const source = read(view.file);
    // Followed by '=' or whitespace, because `includes` alone accepts a
    // longer name that merely starts with this one: renaming the export to
    // window.damgaExecOther satisfied `includes('window.damgaExec')` and the
    // view was reported as published while app.js could not find it. Same
    // shape as the showNewApp assertion below.
    assert.ok(new RegExp(`window\\.${view.global}\\s*=`).test(source),
      `${view.file} does not publish window.${view.global}, so loading it achieves nothing`);
    // Both guards, or the file is a page and not a module — and its own tests
    // stop being able to require it.
    assert.ok(source.includes('module.exports'),
      `${view.file} does not export for node --test`);
  }
});

test('app.js reaches every view, and reaches it by the published name', () => {
  const app = read('app.js');
  for (const view of views) {
    assert.ok(app.includes(view.global),
      `app.js never mentions ${view.global}: ${view.file} would be loaded and never mounted, ` +
      'which is the state logs.js and catalog.js were both in');
  }
});

// The half that made "wired" not the same as "usable". Both views were mounted
// into a page with no rules for them at all: an unstyled <pre> with no lines
// collapses to nothing, so a stream that is connecting or stopped reads as an
// application that printed nothing.
test('the page has rules for what the views draw', () => {
  const css = read('style.css');
  for (const selector of ['.logs', '.catalog-entry', '.refusal', '#catalog-list',
    '.exec-output', '.exec-form', '.settings-row', '.settings-warning', '.tabs',
    '.db-row', '.db-credentials', '.db-form',
    '.source-form', '.source-note']) {
    // Terminated, not merely present. `.exec-outputX { }` contains the string
    // '.exec-output' and would report a rule that styles nothing on the page.
    assert.ok(new RegExp(`\\${selector}(?![\\w-])`).test(css),
      `style.css has no ${selector}: the view renders, and renders unstyled`);
  }
});

// The view that lives in app.js rather than in a file of its own, and can be
// orphaned the same way: a function that draws a form, and no button that
// reaches it. That is not hypothetical here — the panel had exactly one POST
// other than signing in, so the endpoint that registers an application was
// reachable from curl and from nowhere on the page.
test('the page offers a way to create an application, and reaches it', () => {
  const app = read('app.js');
  assert.ok(/function showNewApp\(/.test(app),
    'app.js has no showNewApp: the only way to create an app is the catalogue, which ' +
    'installs a template and cannot register something somebody builds themselves');
  // Two mentions, not one. The definition is a mention, which is why the first
  // version of this assertion passed with the button deleted: `includes` found
  // `showNewApp()` in `function showNewApp()` and reported the view as wired.
  const mentions = app.match(/showNewApp/g) || [];
  assert.ok(mentions.length >= 2,
    'showNewApp is defined and never called: a form nothing opens is a form nobody has');
});

test('the page can actually POST an application', () => {
  const app = read('app.js');
  assert.ok(app.includes('`${tenantBase()}/apps`'),
    "app.js never addresses the tenant's /apps: the form draws and submits nothing, or " +
    'submits somewhere else');
  assert.ok(app.includes('method: "POST"'),
    'nothing on this page POSTs anything but the sign-in form, which is the state that ' +
    'made every first app on every install a curl command');
});

// The exec view can be orphaned the way showNewApp was, and one step earlier.
//
// The table above proves app.js mentions damgaExec — but the mention lives
// inside app.js's own mountExec, so a mountExec that is defined and never
// called satisfies every check above it while the box never appears on the
// page. That is exactly the shape 32 found in the showNewApp assertion, where
// `includes('showNewApp()')` matched the definition and reported a deleted
// button as wired.
//
// So: defined, and called from somewhere that is not its own definition.
test('the exec view is mounted and not merely defined', () => {
  const app = read('app.js');
  assert.ok(/function mountExec\(/.test(app),
    'app.js has no mountExec: exec.js would be loaded by the page and mounted by nothing');
  // Call sites only. A bare count of the word reports three mentions for a
  // view nothing mounts: the definition is one, and `m.mountExec(...)` inside
  // app.js's own seam is another. Deleting the single real call still left two
  // and this assertion passed — which is the showNewApp bug, found again here
  // by deleting the call and watching this line stay green.
  const calls = app.match(/(?<!function )(?<!m\.)mountExec\(/g) || [];
  assert.ok(calls.length >= 1,
    'mountExec is defined and never called: the endpoint is reachable from curl and from ' +
    'nowhere on the page, which is the state logs.js and catalog.js were both in');

  // And it is pushed into the list that gets stopped, or leaving the app
  // abandons a reader on an open POST.
  // [\s\S]*? and not [^)]*: the push already contains mountHealth(...) and
  // mountLogs(...), so a class that stops at the first ')' never reaches this one.
  assert.ok(/mounted\.push\([\s\S]*?mountExec/.test(app),
    'mountExec is called but its stop function is not collected, so switching apps leaves ' +
    'the previous run reading a stream nobody is watching');
});

// The rule the server keeps about its own log, kept by the page.
test('the panel never stores what was typed into the command box', () => {
  const source = read('exec.js');
  for (const forbidden of ['localStorage', 'sessionStorage', 'indexedDB']) {
    assert.ok(!source.includes(forbidden),
      `exec.js uses ${forbidden}: the server deliberately logs the program and not the ` +
      'arguments, because a command line is where a password ends up');
  }
});

// The settings view, and the same orphan check the exec view carries.
//
// Written the way the exec one ended up rather than the way it started: a bare
// count of the word reports a mounted view for one that is only defined,
// because the definition and app.js's own `m.mountSettings(...)` seam are two
// mentions on their own. That is the showNewApp bug, and it was live in three
// assertions in this file before the exec view was added.
test('the settings view is mounted and not merely defined', () => {
  const app = read('app.js');
  assert.ok(/function mountSettings\(/.test(app),
    'app.js has no mountSettings: settings.js would be loaded by the page and mounted by ' +
    'nothing');
  const calls = app.match(/(?<!function )(?<!m\.)mountSettings\(/g) || [];
  assert.ok(calls.length >= 1,
    'mountSettings is defined and never called: the settings endpoint is reachable from ' +
    'curl and from nowhere on the page');
  assert.ok(/mounted\.push\([\s\S]*?mountSettings/.test(app),
    'mountSettings is called but its stop function is not collected, so leaving the app ' +
    'leaves the view behind');
});

// A view nothing navigates to is orphaned even when it is mounted correctly.
//
// showSettings can be defined, wired and unreachable — which is the state the
// new-application form was in, reachable from curl and from nowhere on the
// page. The tab strip is the only way in, so it has to exist and it has to be
// on both views or the way back is missing.
test('both application views are reachable from the other', () => {
  const app = read('app.js');
  assert.ok(/function showSettings\(/.test(app), 'app.js has no showSettings');
  const shown = app.match(/(?<!function )showSettings\b/g) || [];
  assert.ok(shown.length >= 1,
    'showSettings is defined and nothing navigates to it: the settings tab exists in the ' +
    'source and not in the browser');
  assert.ok(/appTabs\(["']overview["']\)/.test(app),
    'the overview does not draw the tab strip, so a reader who opens settings has no way ' +
    'back to it');
  assert.ok(/appTabs\(["']settings["']\)/.test(app),
    'the settings view does not draw the tab strip');
});

// The rule this page inherits from exec.js and from the server's own log.
test('the settings page stores nothing that was typed into it', () => {
  const source = read('settings.js');
  for (const forbidden of ['localStorage', 'sessionStorage', 'indexedDB']) {
    assert.ok(!source.includes(forbidden),
      `settings.js uses ${forbidden}: this is the page where passwords are typed`);
  }
});

// The databases view, and the same two orphan checks the others carry.
test('the databases view is mounted and reachable', () => {
  const app = read('app.js');
  assert.ok(/function mountDatabases\(/.test(app), 'app.js has no mountDatabases');
  const calls = app.match(/(?<!function )(?<!m\.)mountDatabases\(/g) || [];
  assert.ok(calls.length >= 1,
    'mountDatabases is defined and never called: the database endpoint is reachable ' +
    'from curl and from nowhere on the page');
  assert.ok(/mounted\.push\([\s\S]*?mountDatabases/.test(app),
    'mountDatabases is called but its stop function is not collected');

  assert.ok(/function showDatabases\(/.test(app), 'app.js has no showDatabases');
  const shown = app.match(/(?<!function )showDatabases\b/g) || [];
  assert.ok(shown.length >= 1,
    'showDatabases is defined and nothing navigates to it: the tab exists in the source ' +
    'and not in the browser');
  assert.ok(/appTabs\(["']databases["']\)/.test(app),
    'the databases view does not draw the tab strip, so there is no way back from it');
});

test('the source view is mounted and reachable', () => {
  const app = read('app.js');
  assert.ok(/function mountSource\(/.test(app), 'app.js has no mountSource');
  const calls = app.match(/(?<!function )(?<!m\.)mountSource\(/g) || [];
  assert.ok(calls.length >= 1,
    'mountSource is defined and never called: connecting a repository is reachable from ' +
    'curl and from nowhere on the page, which is where this whole chain started');
  assert.ok(/mounted\.push\([\s\S]*?mountSource/.test(app),
    'mountSource is called but its stop function is not collected');
  assert.ok(/function showSource\(/.test(app), 'app.js has no showSource');
  const shown = app.match(/(?<!function )showSource\b/g) || [];
  assert.ok(shown.length >= 1, 'showSource is defined and nothing navigates to it');
  assert.ok(/appTabs\(["']source["']\)/.test(app),
    'the source view does not draw the tab strip, so there is no way back from it');
});
