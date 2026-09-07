// The catalogue: the list a user picks an application from, and the one button.
//
// It decides nothing about whether an entry can be installed, and that is the
// rule this file is under rather than a simplification. Whether n8n installs
// depends on what the converter produced, what the template asks the platform
// to invent, and how many objects the write path can hold — three answers that
// live in the API. A page that worked them out from the entry it can see would
// eventually disagree with the endpoint that refuses, and a catalogue that
// greys out what the API would accept is worse than one that offers everything.
//
// So: the list comes from GET /catalog, and the moment a user picks an entry
// the page asks the install endpoint itself, with dryRun, and shows the answer
// it gets back word for word.

// Turns the filter form into a query string.
//
// Its own function because the parameter names are a contract with the handler
// and getting one wrong fails silently in the worst direction: the endpoint
// ignores what it does not recognise and answers the whole catalogue, which
// reads as a search box that does not work rather than as a bug.
//
// Tags repeat rather than joining with a comma. Two tags narrow, and one
// parameter carrying "a,b" is a single tag nothing has.
function catalogQuery({ text = '', category = '', tags = [] } = {}) {
  const params = new URLSearchParams();
  if (text.trim()) params.set('q', text.trim());
  if (category.trim()) params.set('category', category.trim());
  for (const tag of tags) {
    if (String(tag).trim()) params.append('tag', String(tag).trim());
  }
  const query = params.toString();
  return query ? `?${query}` : '';
}

// Whether the Install button may be offered for this plan.
//
// Strictly true and nothing else. Every other answer — a plan that has not been
// asked for yet, a 503 because no catalogue is mounted, a problem document, a
// response from an older build with no such field — arrives here as something
// that is not true, and all of them mean the same thing: the page does not know
// that this would work, so it must not offer a button that says it will.
function canInstall(plan) {
  return plan !== null && typeof plan === 'object' && plan.installable === true;
}

// The reasons an install was refused, as the API gave them.
//
// Returns an empty list rather than inventing a sentence. The endpoint names
// which of three limits stopped it — a value nothing mints, an image the API
// refuses, more objects than an environment holds — and a page that summarises
// those as "this cannot be installed" throws away the only part a person can
// act on.
function refusals(plan) {
  if (!plan || !Array.isArray(plan.refusals)) return [];
  return plan.refusals;
}

// Where an install would be written, built from what the tenant already has.
//
// The three values this page used to invent — the environment, the namespace
// and the repository — are the three it had no business inventing. It sent
// `prod`, `${app}-prod` and an empty repository, and two of those are worse
// than a blank field: an empty repository is a commit with nowhere to go, and a
// namespace nothing created is a manifest the cluster will refuse to apply
// after the commit has already happened.
//
// So the options come from the placements the tenant already has. A repository
// they have deployed to is a repository that exists and that damga can push to;
// a namespace something is already running in is a namespace that exists. On a
// tenant with nothing deployed there is nothing to offer, and the form says so
// rather than guessing — see the note mountCatalog renders beside it.
function installTargets(apps) {
  const list = Array.isArray(apps) ? apps : [];
  const distinct = (of) => [...new Set(list.map(of).filter(Boolean))].sort();
  return {
    repos: distinct((a) => a.repoUrl),
    namespaces: distinct((a) => a.namespace),
    branches: distinct((a) => a.branch),
  };
}

// The directory the manifest is committed to.
//
// The environment is in the path and that is load bearing rather than tidy:
// two environments sharing one repository, branch and path resolve to the same
// file, and the platform refuses the second — which is right, and which a page
// that offered `apps/${app}` would walk every user straight into.
function defaultPath(app, env) {
  return `apps/${app}/${env}`;
}

// The body of an install, dry run or real.
//
// One function for both, because the only honest way to show somebody what
// will happen is to plan the request that will be sent — not a similar one.
// The page used to dry-run `${app}-prod` and would have installed whatever the
// form said, which is a preview of a different install.
function installBody(entry, form = {}, { dryRun = false } = {}) {
  const app = (form.app || entry.name || '').trim();
  const env = (form.env || '').trim();
  return {
    template: entry.name,
    repoUrl: (form.repoUrl || '').trim(),
    branch: (form.branch || '').trim(),
    path: (form.path || '').trim() || defaultPath(app, env),
    namespace: (form.namespace || '').trim(),
    ...(form.domain ? { domain: form.domain.trim() } : {}),
    ...(dryRun ? { dryRun: true } : {}),
  };
}

// What to say once the button has been pressed.
//
// 201 is the one worth being careful about. It means the placement was written
// and the manifest was committed — it does not mean anything is running, and
// the two are minutes and a whole subsystem apart. A page that said "installed"
// would be believed, and then the reader would go looking for a pod that is not
// there yet and conclude the install failed.
//
// The deploy's own state is quoted rather than described, because the API is
// what knows it and this page has been wrong once already by explaining a
// refusal that had not happened.
function installOutcome(status, body) {
  const detail = (body && body.detail) || '';
  if (status === 201) {
    const deploy = (body && body.deploy) || {};
    const where = (body && body.app) || {};
    return {
      ok: true,
      text: deploy.seq ? `${deploy.seq} numaralı deploy olarak commit edildi` : 'commit edildi',
      // Said in the same breath as the success, not under it.
      note: `${deploy.state ? `Deploy'un durumu: ${deploy.state}. ` : ''}` +
        'Küme commit\'i uygulayana kadar çalışan bir şey olmaz; şuradan izleyin: ' +
        `${where.app || 'uygulama'} · ${where.env || ''}`.trim(),
      reasons: [],
    };
  }
  if (status === 422) {
    // The endpoint refused and listed every reason rather than the first.
    return { ok: false, text: 'bu kurulamaz', note: '', reasons: refusals(body) };
  }
  if (status === 409) {
    // An app that exists, or a repository or namespace another tenant holds.
    // The store's own words name the field and quote only what was sent.
    return { ok: false, text: 'bu zaten alınmış', note: detail, reasons: [] };
  }
  return {
    ok: false,
    text: detail || `kurulum başarısız oldu (HTTP ${status})`,
    note: '',
    reasons: refusals(body),
  };
}

// --- the page ---------------------------------------------------------------

const catalogState = { entries: [], categories: [], selected: null, plan: null };

async function catalogApi(path, options = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  });
  const body = await res.text();
  let parsed = null;
  try {
    parsed = body ? JSON.parse(body) : null;
  } catch {
    parsed = null;
  }
  return { status: res.status, body: parsed, text: body };
}

// planError says what the server said, and names the status when it said
// nothing. "the plan could not be made" would be true of all of them.
function planError(status, body) {
  const detail = body && typeof body.detail === 'string' ? body.detail : '';
  return detail || `plan isteği ${status} ile ve hiçbir ayrıntı vermeden başarısız oldu`;
}

function catalogEl(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else node.setAttribute(k, v);
  }
  for (const child of children) node.append(child);
  return node;
}

// Mounts a section into the page the panel already renders.
//
// It appends rather than expecting markup, so that turning the catalogue on is
// one import in app.js and no edit to index.html. The base path comes from the
// same place the rest of the panel's does — a tenant is in the URL of every
// endpoint here — and is passed in rather than read, because this file does not
// own the panel's state.
function mountCatalog(root, base, options = {}) {
  // What the tenant already has, which is where the form's options come from.
  // Passed in rather than fetched here: app.js has just listed them to draw the
  // navigation, and a second request for the same rows would be a second answer
  // that can differ from the one on screen.
  const targets = installTargets(options.apps);

  const search = catalogEl('input', { id: 'catalog-search', placeholder: 'uygulama ara' });
  const list = catalogEl('div', { id: 'catalog-list' });
  const detail = catalogEl('div', { id: 'catalog-detail' });
  root.append(catalogEl('h2', { text: 'Katalog' }), search, list, detail);

  const load = async () => {
    const { status, body } = await catalogApi(`${base}/catalog${catalogQuery({ text: search.value })}`);
    list.replaceChildren();
    if (status !== 200) {
      // The 503 case says which flag is unset, and it is shown as it arrived:
      // "no applications" and "nobody mounted the catalogue" are different
      // sentences and only the endpoint knows which one is true.
      list.append(catalogEl('p', { class: 'empty', text: (body && body.detail) || 'katalog kullanılamıyor' }));
      return;
    }
    catalogState.entries = body.entries || [];
    catalogState.categories = body.categories || [];
    for (const entry of catalogState.entries) {
      const button = catalogEl('button', { class: 'catalog-entry', type: 'button' },
        catalogEl('strong', { text: entry.name }),
        catalogEl('span', { text: entry.slogan || '' }),
      );
      button.addEventListener('click', () => select(entry));
      list.append(button);
    }
    if (catalogState.entries.length === 0) {
      list.append(catalogEl('p', { class: 'empty', text: 'bu aramayla eşleşen bir şey yok' }));
    }
  };

  const select = async (entry) => {
    catalogState.selected = entry;
    catalogState.plan = null;
    detail.replaceChildren(catalogEl('p', { text: `${entry.name} planlanıyor…` }));

    // The dry run is the panel asking the endpoint that would refuse. Nothing
    // below is computed from the entry.
    //
    // It plans the form's own values, which is the difference between a preview
    // and a preview of something else: this used to dry-run a namespace and a
    // path the install would never have used.
    const form = formValues(entry);
    const { status, body } = await catalogApi(
      `${base}/apps/${encodeURIComponent(form.app)}/envs/${encodeURIComponent(form.env)}/from-catalog`, {
        method: 'POST',
        body: JSON.stringify(installBody(entry, form, { dryRun: true })),
      });

    // The status is read, and it did not used to be. catalogApi returns a
    // status rather than throwing, so a 400 arrived here as a plan: an object
    // with no workloads and no refusals, which render() draws as the entry's
    // name and nothing else. Clicking an entry looked like clicking nothing.
    //
    // A tenant with no apps yet is exactly when it happens — the form defaults
    // come from what the tenant already has, so repoUrl and namespace are empty
    // and the endpoint refuses with "an install needs an app name, an
    // environment and a namespace". That is the first thing a new installation
    // does, and it was the one case with no output.
    if (status !== 200) {
      catalogState.plan = null;
      detail.replaceChildren(
        catalogEl('h3', { text: entry.name }),
        catalogEl('p', { class: 'refusal', text: planError(status, body) }),
      );
      return;
    }
    catalogState.plan = body;
    render(entry, body);
  };

  // The form, and the one place its values are read.
  //
  // Defaults come from what the tenant already has, never from a pattern: the
  // repository and branch of something already deployed are known to work, and
  // the namespace of something already running is known to exist. Only the app
  // name and the environment are the page's own suggestions, and both are
  // names rather than places.
  const fields = {};
  const formValues = (entry) => ({
    app: fields.app ? fields.app.value : entry.name,
    env: fields.env ? fields.env.value : 'prod',
    repoUrl: fields.repoUrl ? fields.repoUrl.value : (targets.repos[0] || ''),
    branch: fields.branch ? fields.branch.value : (targets.branches[0] || 'main'),
    namespace: fields.namespace ? fields.namespace.value : (targets.namespaces[0] || ''),
    path: fields.path ? fields.path.value : '',
  });

  const field = (name, label, value, list) => {
    const input = catalogEl('input', { id: `install-${name}`, value: value || '' });
    if (list && list.length) {
      // A datalist and not a select: what the tenant already has is a good
      // suggestion and never the only legal answer, and a select would make a
      // second repository impossible to type.
      const id = `install-${name}-options`;
      input.setAttribute('list', id);
      const options = catalogEl('datalist', { id });
      for (const v of list) options.append(catalogEl('option', { value: v }));
      fields[`${name}List`] = options;
    }
    fields[name] = input;
    return catalogEl('label', { class: 'install-field' },
      catalogEl('span', { text: label }), input, fields[`${name}List`] || catalogEl('span', {}));
  };

  const render = (entry, plan) => {
    const nodes = [catalogEl('h3', { text: entry.name })];
    if (entry.documentation) {
      nodes.push(catalogEl('a', { href: entry.documentation, text: 'belgeler' }));
    }
    for (const reason of refusals(plan)) {
      nodes.push(catalogEl('p', { class: 'refusal', text: reason }));
    }

    // The form is drawn whether or not the plan can be installed, and it did
    // not used to be: everything below here sat behind an early return on
    // canInstall(). On a tenant with nothing deployed the defaults are empty,
    // because they come from what the tenant already has, so the endpoint
    // refuses with "an install needs an app name, an environment and a
    // namespace" — and the form that supplies those names was hidden by the
    // refusal it exists to clear. Clicking an entry on a new installation
    // showed a refusal and no way to answer it.
    //
    // The note about namespaces was behind the same return, which is the sharp
    // part: it was written for a tenant with no namespaces and was invisible
    // to exactly that tenant.
    //
    // What the plan gates now is the button, which is the thing that should
    // not be pressed while the endpoint is refusing.
    const guess = formValues(entry);
    const form = catalogEl('div', { class: 'install-form' },
      field('app', 'Ad', guess.app),
      field('env', 'Ortam', guess.env),
      field('repoUrl', 'Manifest\'leri için depo', guess.repoUrl, targets.repos),
      field('branch', 'Dal', guess.branch, targets.branches),
      field('namespace', 'Namespace', guess.namespace, targets.namespaces),
    );
    nodes.push(form);

    if (targets.namespaces.length === 0) {
      // Said rather than filled in. Nothing in this platform creates a
      // namespace — checked, and there is no Go that builds one — so a name
      // this page invented would be committed, applied, and refused by the
      // cluster after the commit had already happened. A tenant with something
      // deployed has a namespace to offer; the first install does not, and the
      // person doing it is the one who knows which one exists.
      nodes.push(catalogEl('p', { class: 'install-note', text:
        'Buradaki hiçbir şey namespace oluşturmaz. Bu, kümede zaten var olan, üzerinde ' +
        'kiracı kotası ve Pod Security etiketleri bulunan bir namespace olmalı.' }));
    }
    if (targets.repos.length === 0) {
      nodes.push(catalogEl('p', { class: 'install-note', text:
        'Manifest\'ler, damga\'nın push edebildiği bir depoya commit edilir. Bu kiracının ' +
        'kayıtlı deposu henüz yok, bu yüzden adresin yazılması gerekiyor.' }));
    }

    // Changing a field re-plans. Without this the form clears the refusal in
    // the reader's head and nowhere else: they would type a namespace, see the
    // same refusal, and have nothing to press.
    for (const name of ['app', 'env', 'repoUrl', 'branch', 'namespace', 'path']) {
      if (fields[name]) fields[name].addEventListener('change', () => { select(entry).catch(() => {}); });
    }

    if (!canInstall(plan)) {
      nodes.push(catalogEl('p', { class: 'install-note', text:
        'Bir alan değiştiğinde plan yeniden kontrol edilir.' }));
      detail.replaceChildren(...nodes);
      return;
    }

    const status = catalogEl('p', { class: 'install-status' });
    const button = catalogEl('button', { class: 'install', type: 'button', text: `${entry.name} kur` });

    button.addEventListener('click', async () => {
      const values = formValues(entry);
      const missing = ['app', 'env', 'repoUrl', 'namespace'].filter((k) => !values[k].trim());
      if (missing.length) {
        // Refused here rather than sent, because the endpoint's answer to an
        // empty repository is a 400 about a field the reader cannot see.
        status.className = 'install-status bad';
        status.textContent = `hâlâ gerekli: ${missing.join(', ')}`;
        return;
      }
      button.disabled = true;
      status.className = 'install-status';
      status.textContent = `${entry.name} kuruluyor…`;

      const { status: code, body } = await catalogApi(
        `${base}/apps/${encodeURIComponent(values.app)}/envs/${encodeURIComponent(values.env)}/from-catalog`, {
          method: 'POST',
          body: JSON.stringify(installBody(entry, values)),
        });
      const outcome = installOutcome(code, body);

      status.className = `install-status ${outcome.ok ? 'ok' : 'bad'}`;
      status.textContent = outcome.text;
      // Everything the outcome carries, under the sentence rather than instead
      // of it: a refused install lists every reason, and a successful one says
      // in the same breath that nothing is running yet.
      const extra = [];
      if (outcome.note) extra.push(catalogEl('p', { class: 'install-note', text: outcome.note }));
      for (const reason of outcome.reasons) {
        extra.push(catalogEl('p', { class: 'refusal', text: reason }));
      }
      status.after(...extra);
      button.disabled = outcome.ok;
      if (outcome.ok && typeof options.onInstalled === 'function') options.onInstalled(body);
    });

    nodes.push(button, status);
    detail.replaceChildren(...nodes);
  };

  search.addEventListener('input', () => { load().catch(() => {}); });
  return load;
}

// The two guards that let this file be both a page and a module, for the reason
// app.js gives: the browser has document and no module, node --test has the
// reverse, and the panel has no build step to reconcile them.
if (typeof window !== 'undefined') {
  window.damgaCatalog = {
    catalogQuery, canInstall, refusals, planError, mountCatalog,
    installTargets, defaultPath, installBody, installOutcome,
  };
}

if (typeof module !== 'undefined') {
  module.exports = {
    catalogQuery, canInstall, refusals, planError, mountCatalog,
    installTargets, defaultPath, installBody, installOutcome,
  };
}
