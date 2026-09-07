// The panel. It renders what the API says and decides nothing itself: no
// verdict here is computed locally, because a panel that can reach a different
// conclusion from the CLI about the same deploy is worse than no panel.

const state = { account: null, memberships: [], tenant: null, ref: null, apps: [] };

// The 401 case, given a name. Without one, "the session ended" and "the page
// has a bug" are the same rejected promise, and every failure renders as the
// sign-in form — which says the one thing guaranteed to send somebody looking
// in the wrong place. This cost an hour the first time.
class NotSignedIn extends Error {}

const $ = (id) => document.getElementById(id);

// Whether anything actually recorded an admission outcome for this record.
//
// The panel decides nothing, and this is the smallest amount of deciding that
// still honours that: it does not work out whether a deploy was admitted, only
// whether anybody said. A record carrying no reason, not allowed, and not in
// the rejected state is one nothing has observed — and saying "refused" about
// it would be the panel reaching a conclusion the API never gave it.
// What the page shows when there is no running deploy, as a value rather than
// an expression buried in a render function.
//
// Pulled out because this is the one place the page decides something, and the
// one time it decided wrongly nothing could have caught it: the panel has no
// test tier, and the bug — every deploy that had not finished syncing rendering
// as "was refused", under a banner explaining the refusal — sat in an inline
// ternary for as long as the page existed.
//
// A deploy admission refused never becomes current, so the page would otherwise
// say "nothing is running" and stop, with the reason sitting in the very next
// record down. Refusal is read from the state, which is somewhere a record
// arrives because something moved it there. It used to be read from
// !latest.admission.allowed, and nothing wrote that field — so it was false on
// every record ever created, and the absence of an observation was shown as the
// worst thing that could have happened.
const whatToShow = (current, latest) => {
  const blocked = Boolean(!current && latest && latest.state === "rejected");
  return { blocked, shown: current || (blocked ? latest : null) };
};

// What the backup box says, as plain values. No elements: the decision is which
// sentence is true, and building it out of DOM nodes puts the one thing worth
// testing behind a browser.
//
// Three states and not two. "Restored" is the claim the whole rehearsal exists
// to support; "backed up" is the weaker one an install that turned the
// rehearsal off is entitled to, and saying it is more honest than a blank line;
// "none yet" is a database whose first run has not happened, which is neither a
// success nor a failure and would otherwise have to be rendered as one.
//
// The count is given against the source and never alone. "1,284 rows came back"
// and "1,284 came back out of 1,284" are different claims, and only the second
// is what was measured.
const backupView = (b) => {
  if (b.state === "none yet") {
    return {
      database: b.database || "—",
      lastRun: { text: "henüz hiçbir yedek çalışmadı", ok: null },
      verified: null,
    };
  }
  const restored = b.state === "restored";
  return {
    database: b.database || "—",
    lastRun: {
      text: `${restored ? "geri yüklendi" : "yedeklendi"} ${when(b.finishedAt)}`,
      ok: restored,
    },
    verified: restored
      ? {
          text: `${b.sourceRows} satırın ${b.rows} tanesi, ` +
            `${b.tables} tablo üzerinden`,
          ok: true,
        }
      : { text: "arşiv yazıldı, geri yüklenmedi", ok: null },
  };
};

const admissionKnown = (r) =>
  Boolean(r.admission.allowed || r.admission.reason || r.state === "rejected");

// One place that talks to the API, so the signed-out case is handled once.
// Anything can 401 at any time — a session expires, an account is disabled
// while somebody has the page open — and the answer is always the same.
async function api(path, options = {}) {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "Accept": "application/json", ...(options.headers || {}) },
    ...options,
  });
  if (res.status === 401) {
    throw new NotSignedIn("giriş yapılmadı");
  }
  const body = res.headers.get("content-type")?.includes("json")
    ? await res.json()
    : null;
  if (!res.ok) {
    throw Object.assign(new Error(body?.detail || `HTTP ${res.status}`), { status: res.status });
  }
  return body;
}

// ---------------------------------------------------------------- sign in

function showSignIn() {
  $("app").hidden = true;
  $("signin").hidden = false;
}

function wireSignIn() {
$("signin-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target;
  const button = form.querySelector("button");
  const error = $("signin-error");
  error.hidden = true;
  button.disabled = true;
  try {
    await api("/api/v1/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        email: form.email.value,
        password: form.password.value,
      }),
    });
    form.password.value = "";
    await start();
  } catch (err) {
    // Whatever the server said, verbatim. It deliberately says the same thing
    // for a wrong password and an address that does not exist, and rewording
    // it here could undo that.
    error.textContent = err.message;
    error.hidden = false;
    if (!(err instanceof NotSignedIn)) {
      console.error("damga: sign-in failed after the credentials were accepted", err);
    }
  } finally {
    button.disabled = false;
  }
});

}

function wireSignOut() {
$("signout").addEventListener("click", async () => {
  try {
    await fetch("/api/v1/logout", { method: "POST", credentials: "same-origin" });
  } finally {
    location.reload();
  }
});
}

// ---------------------------------------------------------------- shell

async function start() {
  const me = await api("/api/v1/me");
  state.account = me.account;
  state.memberships = me.memberships;

  $("signin").hidden = true;
  $("app").hidden = false;
  $("who").textContent = me.account.displayName || me.account.email;

  const select = $("tenant");
  select.replaceChildren();
  for (const m of me.memberships) {
    const option = document.createElement("option");
    option.value = m.tenantId;
    option.textContent = `${m.tenantName} · ${m.role}`;
    select.append(option);
  }
  if (!me.memberships.length) {
    $("apps").replaceChildren();
    render(el("p", { class: "muted empty" },
      "Bu hesap henüz hiçbir kiracının üyesi değil."));
    return;
  }
  select.value = me.memberships[0].tenantId;
  await pickTenant(select.value);
}

function wireTenant() {
  $("tenant").addEventListener("change", (e) => pickTenant(e.target.value));
}

async function pickTenant(tenantId) {
  state.tenant = tenantId;
  state.ref = null;
  const { apps } = await api(`/api/v1/tenants/${encodeURIComponent(tenantId)}/apps`);
  // Kept because the catalogue's install form is built from them: a repository
  // this tenant has already deployed to is one that exists and that damga can
  // push to, and a namespace something is already running in is one that
  // exists. Re-fetching them there would be a second answer that can differ
  // from the list on screen.
  state.apps = apps;

  const nav = $("apps");
  nav.replaceChildren();

  // Above the list and present whether or not there is one. A tenant with
  // nothing deployed is exactly the tenant that wants this, and the empty
  // branch below used to return before anything else was offered — which left
  // the one page that can create an app reachable only from a tenant that
  // already had one.
  const catalogue = el("button", { type: "button", class: "catalogue" }, "Uygulama kur");
  catalogue.addEventListener("click", () => showCatalog());
  nav.append(catalogue);

  // The other way in, and until now the only one that did not exist. The
  // catalogue installs something this platform ships a template for; this
  // registers an application somebody builds themselves, which is the case the
  // panel had no path to at all. Measured before it was written: the only POST
  // this page made, other than signing in, was /api/v1/login — so every first
  // app on every install had to be created with curl, and the endpoint's own
  // refusals (the one that names -git-token-file among them) were sentences
  // nobody could reach from here.
  const newApp = el("button", { type: "button", class: "catalogue" }, "Uygulama ekle");
  newApp.addEventListener("click", () => showNewApp());
  nav.append(newApp);

  if (!apps.length) {
    nav.append(el("p", { class: "muted" }, "Buraya henüz hiçbir şey deploy edilmedi."));
    render(el("p", { class: "muted empty" },
      "Bir şey deploy edildiğinde, kanıtıyla birlikte burada görünür."));
    return;
  }
  for (const app of apps) {
    const button = el("button", { type: "button" },
      el("span", {}, app.app), " ", el("span", { class: "env" }, app.env));
    button.addEventListener("click", () => pickApp(app));
    nav.append(button);
  }
  await pickApp(apps[0]);
}

// The stop functions of everything currently mounted.
//
// One list rather than a variable per view, because the rule is the same for
// all of them and it is easy to add a view and forget it: leaving an app for
// another one, or for the catalogue, has to close what the last one opened. A
// log stream that is not closed is a connection per app somebody has looked at,
// all of them still held open on the server.
let mounted = [];

function stopMounted() {
  for (const stop of mounted) {
    if (typeof stop === "function") stop();
  }
  mounted = [];
}

// The seams onto the three files that are loaded by plain script tags and
// publish themselves on window.
//
// Absent is handled rather than assumed. The panel has no build step, so a
// missing script tag is a missing global — and a page that throws on one is a
// page with no evidence view either. Each of these leaves its box empty and the
// rest of the page renders.
function mountFrom(globalName, mount) {
  const module = typeof window !== "undefined" ? window[globalName] : null;
  if (!module) return null;
  return mount(module);
}

function mountHealth(el, prefix) {
  return mountFrom("damgaMetrics", (m) => m.mountMetrics(el, prefix, { fetcher: api }));
}

// The log view. It returns its own stop function and manages its own
// reconnects; what it needs from here is somewhere to draw and the base URL.
function mountLogs(el, prefix) {
  return mountFrom("damgaLogs", (m) => m.mountLogs(el, prefix, {}));
}

// The settings view. Same raw-Response fetcher the exec view needs, and for
// the first of the same two reasons: api() throws on a non-2xx, and this page
// prints the endpoint's refusal rather than a generic failure — so it has to
// see the body of a 400.
function mountSettings(el, prefix) {
  return mountFrom("damgaSettings", (m) => m.mountSettings(el, prefix, {
    fetcher: async (path, options = {}) => {
      const res = await fetch(path, {
        credentials: "same-origin",
        ...options,
        headers: { "Accept": "application/json", ...(options.headers || {}) },
      });
      if (res.status === 401) throw new NotSignedIn("giriş yapılmadı");
      return res;
    },
  }));
}

// The databases view. Raw Response for the reason the settings view needs one:
// this page prints the endpoint's refusal, so it has to see the body of a 400.
function mountDatabases(el, prefix) {
  return mountFrom("damgaDatabases", (m) => m.mountDatabases(el, prefix, {
    fetcher: async (path, options = {}) => {
      const res = await fetch(path, {
        credentials: "same-origin",
        ...options,
        headers: { "Accept": "application/json", ...(options.headers || {}) },
      });
      if (res.status === 401) throw new NotSignedIn("giriş yapılmadı");
      return res;
    },
  }));
}

// Three views over one application: what it did, what it is set to, and what
// it stores.
//
// A strip rather than a second entry in the app list, because they are the
// same app and the list is how you choose which app. The current one is a
// span and not a button: a control that does nothing is a control somebody
// presses twice wondering what they missed.
function appTabs(active) {
  const strip = el("nav", { class: "tabs", "aria-label": "Uygulama görünümleri" });
  for (const tab of [
    { id: "overview", label: "Genel bakış", go: showEvidence },
    { id: "settings", label: "Ayarlar", go: showSettings },
    { id: "databases", label: "Veritabanları", go: showDatabases },
  ]) {
    if (tab.id === active) {
      const here = el("span", { class: "tab", "aria-current": "true" }, tab.label);
      strip.append(here);
      continue;
    }
    const button = el("button", { type: "button", class: "tab" }, tab.label);
    button.addEventListener("click", () => tab.go());
    strip.append(button);
  }
  return strip;
}

// The settings view: environment variables, with the build/runtime split and
// the secrets that are deliberately not shown.
//
// This is the gap that stopped n8n installing from the catalogue — the
// template needs a token and there was no way to type one. docs/PLAN.md §6.
function showSettings() {
  const prefix = base();
  stopMounted();
  const { app, env } = state.ref;
  const box = el("div", {});
  render(
    appTabs("settings"),
    el("h2", {}, `${app} · ${env}`),
    el("p", { class: "sub muted" },
      "Ortam değişkenleri. Düz değerler git'e commit edilir; sır değerleri kümeye " +
      "yazılır ve commit edilmez."),
    box,
  );
  mounted.push(mountSettings(box, prefix));
}

// The databases view: one click, and a Postgres or a Redis exists.
//
// The engine was always here — the Database CRD carries both, with backups and
// a restore rehearsal — and the only way to ask for one was kubectl.
// docs/PLAN.md §7.
function showDatabases() {
  const prefix = base();
  stopMounted();
  const { app, env } = state.ref;
  const box = el("div", {});
  render(
    appTabs("databases"),
    el("h2", {}, `${app} · ${env}`),
    el("p", { class: "sub muted" },
      "Postgres ve Redis, her şey gibi git'e commit edilir. Kimlik bilgileri, " +
      "uygulamanızın okuduğu bir Secret'a üretilir ve asla git'te bulunmaz."),
    box,
  );
  mounted.push(mountDatabases(box, prefix));
}

// The exec view, and the one seam here that cannot use api().
//
// api() reads the body into JSON and throws on a non-2xx, which is right for
// every other endpoint and wrong for this one twice over: the body is a stream
// this view reads frame by frame, and a command that exits 1 comes back 200
// with the code in the last frame. Handing it to api() would consume the
// stream and lose the exit code. What is kept by hand is the part that
// matters here — a 401 still becomes the sign-in form.
function mountExec(el, prefix) {
  return mountFrom("damgaExec", (m) => m.mountExec(el, prefix, {
    fetcher: async (path, options = {}) => {
      const res = await fetch(path, {
        credentials: "same-origin",
        ...options,
        headers: { "Accept": "text/event-stream", ...(options.headers || {}) },
      });
      if (res.status === 401) throw new NotSignedIn("giriş yapılmadı");
      return res;
    },
  }));
}

async function pickApp(app) {
  state.ref = app;
  stopMounted();
  for (const button of $("apps").children) {
    const match = button.textContent === `${app.app} ${app.env}`;
    button.setAttribute("aria-current", String(match));
  }
  await showEvidence();
}

// ---------------------------------------------------------------- catalogue

// The catalogue is the tenant's, not an app's, so it is mounted over the whole
// detail pane rather than into a box beside a deploy record.
//
// Nothing here decides what can be installed. catalog.js asks the install
// endpoint with dryRun and prints the refusals it gets back word for word,
// which is the same rule the health view is under: the page shows what the API
// said, and the moment it works out an answer for itself the page and the
// endpoint can disagree about the same template.
function showCatalog() {
  stopMounted();
  for (const button of $("apps").children) {
    if (button.setAttribute) button.setAttribute("aria-current", "false");
  }
  state.ref = null;

  const root = el("div", { class: "catalogue-view" });
  render(root);

  const load = mountFrom("damgaCatalog", (m) => m.mountCatalog(root, tenantBase(), {
    apps: state.apps,
    // An install writes a placement, so the navigation beside it is out of date
    // the moment one succeeds. Reloading the tenant is what makes the new app
    // appear where every other app is — and it is deliberately not a jump into
    // it: the commit is made and nothing is running yet, and moving the reader
    // somewhere that says "nothing has been deployed here" would read as the
    // install having failed.
    onInstalled: () => { pickTenant(state.tenant).catch(fail); },
  }));
  if (!load) {
    // The script tag is missing. Said rather than left as a blank pane, for
    // the reason the health view leaves its box empty and the page still
    // renders: a view that silently is not there looks like a catalogue with
    // nothing in it.
    render(el("p", { class: "muted empty" },
      "Katalog görünümü yüklenmedi; /catalog.js sunulmuyor."));
    return;
  }
  load().catch(fail);
}

// ------------------------------------------------------------- new application

// The body of POST /apps, as values rather than as a request.
//
// A function of its own because it is the only part of this form that decides
// anything, and the panel's rule is that what decides is testable. Trimming is
// not cosmetic: the endpoint refuses " prod" as an environment with a message
// about Kubernetes names, which reads as the platform rejecting the word prod.
//
// The path is defaulted here and not left empty, and the default is the
// catalogue's: apps/<app>/<env>. The environment has to be in it, because two
// environments that resolve to one directory are one environment with two
// names — the store carries a unique index that refuses the second, and the
// person typing would have no idea why.
function newAppBody(form = {}) {
  const app = (form.app || "").trim();
  const env = (form.env || "").trim();
  return {
    app,
    env,
    repoUrl: (form.repoUrl || "").trim(),
    branch: (form.branch || "").trim(),
    path: (form.path || "").trim() || `apps/${app}/${env}`,
    namespace: (form.namespace || "").trim(),
  };
}

// What to say once the app has been created, or refused.
//
// The delivery line is quoted and never summarised. It is the sentence that
// says whether anything will apply this app's commits — "an Argo CD Application
// is watching this app's directory", or that with a "but" naming the credential
// Argo CD was not given, or that nothing is applying anything because this
// install was started without a cluster. All three are the server's own words
// about a part of the system this page cannot see, and a panel that replaced
// them with "created" would be hiding the only interesting half of the answer.
function newAppOutcome(status, body) {
  const detail = (body && body.detail) || "";
  if (status === 201) {
    const app = (body && body.app) || {};
    return {
      ok: true,
      text: `${app.app || "uygulama"} · ${app.env || ""}`.trim() + " kaydedildi",
      // Said in the same breath as the success. Registering an app writes a
      // placement and an Argo CD Application; it deploys nothing, and somebody
      // who read "registered" as "running" would go looking for a pod that was
      // never asked for.
      note: (body && body.delivery) || "",
      hint: "Bu hiçbir şey deploy etmez. Bu uygulamanın manifest'lerinin nereye commit "
        + "edildiğini ve hangi namespace'te çalıştıklarını kaydeder.",
    };
  }
  if (status === 409) {
    // An app that exists, or a repository or namespace another tenant holds.
    // The store's words name the field and quote only what was sent.
    return { ok: false, text: "bu zaten alınmış", note: detail, hint: "" };
  }
  return { ok: false, text: detail || `uygulama oluşturulamadı (HTTP ${status})`, note: "", hint: "" };
}

// The form itself.
//
// Its defaults come from what the tenant already has, for the reason the
// catalogue's do: a repository something is already deployed to is one that
// exists and that damga can push to, and a namespace something is running in is
// one that exists. The difference here is that the first app on an install has
// neither, which is the whole case this view was missing — so nothing is
// required to be a choice from a list, every field is typed, and the notes say
// what has to be true of the values rather than the page pretending to know.
function showNewApp() {
  stopMounted();
  for (const button of $("apps").children) {
    if (button.setAttribute) button.setAttribute("aria-current", "false");
  }
  state.ref = null;

  const known = (of) => [...new Set((state.apps || []).map(of).filter(Boolean))].sort();
  const fields = {};
  const field = (name, label, value, options = []) => {
    const input = el("input", { id: `newapp-${name}`, value: value || "" });
    let listNode = el("span", {});
    if (options.length) {
      // A datalist and not a select, the same choice the catalogue's form
      // makes: what the tenant already has is a suggestion and never the only
      // legal answer.
      const id = `newapp-${name}-options`;
      input.setAttribute("list", id);
      listNode = el("datalist", { id }, ...options.map((v) => el("option", { value: v })));
    }
    fields[name] = input;
    return el("label", { class: "install-field" }, el("span", {}, label), input, listNode);
  };

  const status = el("p", { class: "install-status" });
  const button = el("button", { class: "install", type: "button" }, "Uygulamayı ekle");

  const form = el("div", { class: "install-form" },
    field("app", "Ad", ""),
    field("env", "Ortam", "prod"),
    field("repoUrl", "Manifest'leri için depo", known((a) => a.repoUrl)[0] || "", known((a) => a.repoUrl)),
    field("branch", "Dal", known((a) => a.branch)[0] || "main", known((a) => a.branch)),
    field("path", "O depodaki dizin", ""),
    field("namespace", "Namespace", "", known((a) => a.namespace)),
  );

  const values = () => Object.fromEntries(
    Object.entries(fields).map(([name, input]) => [name, input.value]));

  button.addEventListener("click", async () => {
    const body = newAppBody(values());
    const missing = ["app", "env", "repoUrl", "namespace"].filter((k) => !body[k]);
    if (missing.length) {
      // Refused here rather than sent, because the endpoint answers an empty
      // repository with a 400 naming a field this form calls something else.
      status.className = "install-status bad";
      status.textContent = `hâlâ gerekli: ${missing.join(", ")}`;
      return;
    }
    button.disabled = true;
    status.className = "install-status";
    status.textContent = "oluşturuluyor…";
    let outcome;
    try {
      const created = await api(`${tenantBase()}/apps`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      outcome = newAppOutcome(201, created);
    } catch (err) {
      if (err instanceof NotSignedIn) {
        fail(err);
        return;
      }
      // api() throws the detail as the message and keeps the status on the
      // error, which is everything the outcome needs; there is no second
      // request here to get the body back.
      outcome = newAppOutcome(err.status || 0, { detail: err.message });
    }

    status.className = `install-status ${outcome.ok ? "ok" : "bad"}`;
    status.textContent = outcome.text;
    const extra = [];
    if (outcome.note) extra.push(el("p", { class: "install-note" }, outcome.note));
    if (outcome.hint) extra.push(el("p", { class: "muted" }, outcome.hint));
    status.after(...extra);
    button.disabled = outcome.ok;
    if (outcome.ok) {
      // The navigation is out of date the moment this succeeds. Reloaded
      // rather than jumped into, for the reason the catalogue gives: the app
      // exists and nothing is running in it, and landing the reader on a page
      // that says "nothing has been deployed here" reads as a failure.
      pickTenant(state.tenant).catch(fail);
    }
  });

  // The catalogue's own wrapper, because the page's rules for a full-pane view
  // are written against it. A bare h2 in #detail is the heading of an evidence
  // box and is sized like one.
  render(el("div", { class: "catalogue-view" },
    el("h2", {}, "Uygulama ekle"),
    el("p", { class: "muted" },
      "Kendi yazdığınız bir şey için. Katalog, bu platformun şablon getirdiği şeyler "
      + "içindir."),
    form,
    el("p", { class: "install-note" },
      "Depo, damga'nın bu uygulamanın manifest'lerini commit ettiği yerdir — kaynağının "
      + "yaşadığı yer değil. Zaten var olmalı ve damga oraya push edebilmeli; buradaki "
      + "hiçbir şey depo oluşturmaz."),
    el("p", { class: "install-note" },
      "Namespace'in de var olması gerekiyor, üzerinde kiracı kotası ve Pod Security "
      + "etiketleriyle birlikte."),
    button,
    status,
  ));
}

// ---------------------------------------------------------------- evidence

// The tenant's own root. The catalogue lives here rather than under an app,
// because it is what you use before there is one.
function tenantBase() {
  return `/api/v1/tenants/${encodeURIComponent(state.tenant)}`;
}

function base() {
  const { tenant, ref } = state;
  return `/api/v1/tenants/${encodeURIComponent(tenant)}` +
    `/apps/${encodeURIComponent(ref.app)}/envs/${encodeURIComponent(ref.env)}`;
}

async function showEvidence() {
  const prefix = base();
  render(el("p", { class: "muted empty" }, "Yükleniyor…"));

  // Fetched together rather than in sequence. They are independent reads and
  // the page is not useful until all of them are in.
  const [current, history, proof, retention, backup] = await Promise.all([
    api(`${prefix}/evidence`).catch((err) => (err.status === 404 ? null : Promise.reject(err))),
    api(`${prefix}/history?limit=20`),
    api(`${prefix}/verify`),
    api(`${prefix}/retention`),
    // Absent for an app with no database, and for an install with no cluster to
    // read. Both are ordinary and neither is a reason to fail the page, so the
    // whole block is left out rather than rendered as a failure.
    api(`${prefix}/backup`).catch(() => null),
  ]);

  const { app, env } = state.ref;

  // What to show when nothing is running. A deploy that admission refused
  // never becomes current, so this page would otherwise say "nothing is
  // running" and stop — with the reason it is not running sitting in the very
  // next record down. On a product whose entire claim is that it can tell you
  // why, that is the wrong place to go quiet.
  const latest = history.records[0] || null;
  const { blocked, shown } = whatToShow(current, latest);

  const parts = [
    appTabs("overview"),
    el("h2", {}, `${app} · ${env}`),
    el("p", { class: "sub muted" }, current
      ? `Deploy ${current.seq}, ${current.state}, ${when(current.updatedAt)}`
      : blocked
        ? `Çalışan bir şey yok. ${latest.seq} numaralı deploy reddedildi, ${when(latest.createdAt)}.`
        : latest
          ? `Çalışan bir şey yok. ${latest.seq} numaralı deploy ${latest.state}, ${when(latest.createdAt)}.`
          : "Buraya henüz hiçbir şey deploy edilmedi."),
  ];

  if (blocked) {
    parts.push(el("div", { class: "box banner" },
      el("h3", {}, "Reddedildi"),
      el("p", {}, latest.admission.reason || "Admission bu deploy'u reddetti.")));
  }

  if (shown) {
    parts.push(el("div", { class: "grid" },
      box("İmaj", dl([
        ["Çalışan", el("span", { class: "mono" }, shown.image.requestedRef || "—")],
      ])),
      box("Kaynak", dl([
        ["Depo", shown.source.repoUrl || "—"],
        ["Commit", el("span", { class: "mono" }, shown.source.commitSha || "—")],
        ["Yol", el("span", { class: "mono" }, shown.source.path || "—")],
        ["Deploy eden", shown.actor.displayName || shown.actor.email || "—"],
      ])),
      box("Admission", dl([
        // Three answers and not two. Nothing records an admission outcome yet,
        // so a page with only "admitted" and "refused" has to call every deploy
        // in existence refused — which is a claim, made out of a zero value,
        // about the one thing this page exists to be trusted on.
        ["Sonuç", admissionKnown(shown)
          ? verdict(shown.admission.allowed,
              shown.admission.allowed ? "kabul edildi" : "reddedildi")
          : el("span", { class: "muted" }, "gözlemlenmedi")],
        ["Gerekçe", shown.admission.reason || "—"],
      ])),
    ));
  }

  if (backup && backup.backup) {
    const view = backupView(backup.backup);
    const cell = (v) =>
      v.ok === null ? el("span", { class: "muted" }, v.text) : verdict(v.ok, v.text);
    const rows = [["Veritabanı", view.database], ["Son çalışma", cell(view.lastRun)]];
    if (view.verified) {
      rows.push(["Doğrulandı", cell(view.verified)]);
    }
    parts.push(el("div", { class: "box", style: "margin-top:1rem" },
      el("h3", {}, "Yedek"), dl(rows)));
  }

  parts.push(el("div", { class: "box", style: "margin-top:1rem" },
    el("h3", {}, "Zincir"),
    dl([
      // Reported, never recomputed here. The server verified it; this says so.
      ["Doğrulandı", verdict(proof.valid,
        proof.valid
          ? `${proof.records} kayıt sağlam`
          : `${proof.brokenAt} noktasında kırık`)],
      ["Kök hash", el("span", { class: "mono" }, short(proof.rootHash))],
      ["Kontrol edildi", when(proof.checkedAt)],
      ["Saklama", retention.windowSeconds
        ? `${Math.round(retention.windowSeconds / 86400)} gün, çalışan deploy saklanır`
        : "süresiz saklanır"],
    ])));

  parts.push(el("div", { class: "box", style: "margin-top:1rem" },
    el("h3", {}, "Geçmiş"),
    history.records.length
      ? table(["#", "Durum", "İmaj", "Commit", "Ne zaman"],
          history.records.map((r) => [
            String(r.seq), r.state, r.image.requestedRef || "—",
            el("span", { class: "mono" }, short(r.source.commitSha)), when(r.createdAt),
          ]))
      : el("p", { class: "muted" }, "Henüz kaydedilmiş deploy yok.")));

  parts.push(el("div", { class: "actions" },
    el("a", { href: `${prefix}/export`, download: "" }, "Tüm kaydı indir (JSONL)"),
    el("span", { class: "muted" }, "Her kayıt, en eskisi önce, zincirin üzerinde hesaplandığı biçimde.")));

  // The health box, mounted rather than rendered, because it reloads on its own
  // button and the rest of this page does not.
  //
  // Last on the page and first in what people come for, which is the ordering
  // this file already uses: the deploy record answers "what did I ship" and
  // this answers "why is it dying", and the second question is asked while the
  // first is already known.
  const health = el("div", { class: "box", style: "margin-top:1rem" });
  parts.push(health);

  // And what the process is writing, under what the platform knows about it.
  // Last because it is the only thing on the page that keeps arriving.
  const logs = el("div", { class: "box", style: "margin-top:1rem" },
    el("h3", {}, "Loglar"));
  parts.push(logs);

  // And the one box on this page that does something rather than showing
  // something. Below the logs on purpose: the command somebody runs here is
  // usually the answer to what the logs above it just said.
  const run = el("div", { class: "box", style: "margin-top:1rem" },
    el("h3", {}, "Komut çalıştır"),
    el("p", { class: "muted" },
      "Bir kez, tek bir konteynerde, uygulama kullanıcısı olarak çalışır. Yalnızca sahip."));
  parts.push(run);

  render(...parts);

  // After render, because replaceChildren would otherwise drop what mount put
  // in. api and not fetch: it is the one place a 401 becomes the sign-in form.
  const logBody = el("div", {});
  logs.append(logBody);
  const execBody = el("div", {});
  run.append(execBody);
  mounted.push(mountHealth(health, prefix), mountLogs(logBody, prefix), mountExec(execBody, prefix));
}

// ---------------------------------------------------------------- rendering

function render(...nodes) {
  $("detail").replaceChildren(...nodes);
}

// Builds elements without innerHTML, so a repository URL or a policy message
// that happens to contain markup is text and stays text.
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  node.append(...children.flat());
  return node;
}

function box(title, ...body) {
  return el("div", { class: "box" }, el("h3", {}, title), ...body);
}

function dl(rows) {
  return el("dl", {}, rows.flatMap(([k, v]) => [el("dt", {}, k), el("dd", {}, v)]));
}

function table(headers, rows) {
  return el("table", {},
    el("thead", {}, el("tr", {}, headers.map((h) => el("th", {}, h)))),
    el("tbody", {}, rows.map((cells) => el("tr", {}, cells.map((c) => el("td", {}, c))))));
}

function verdict(ok, text) {
  return el("span", { class: `pill ${ok ? "ok" : "bad"}` }, text);
}

function short(hex) {
  if (!hex) return "—";
  return hex.length > 16 ? `${hex.slice(0, 12)}…` : hex;
}

function when(stamp) {
  if (!stamp) return "—";
  const date = new Date(stamp);
  return Number.isNaN(date.valueOf()) ? stamp : date.toLocaleString();
}

// ---------------------------------------------------------------- boot

// No cookie check first: whether the session is good is the server's answer,
// and asking is one request either way.
//
// Only a 401 becomes the sign-in form. Anything else is shown as what it is,
// because a bug that renders as "please sign in" is a bug nobody reports
// accurately — they say the login is broken, and it is not.
function fail(err) {
  if (err instanceof NotSignedIn) {
    showSignIn();
    return;
  }
  console.error("damga:", err);
  $("signin").hidden = true;
  $("app").hidden = false;
  render(
    el("h2", {}, "Bu sayfa yüklenemedi"),
    el("p", { class: "error" }, String(err && err.message ? err.message : err)),
    el("p", { class: "muted" }, "Oturum sorunsuz. Bu, panelde ya da arkasındaki API'de bir hata."),
  );
}

// Wiring, and the two guards that let this file be both a page and a module.
//
// The page loads it in a browser, where document exists and module does not.
// panel/app_test.js requires it under node --test, where the reverse is true —
// no DOM, no npm, nothing installed. Without the first guard the file throws on
// import at the first getElementById; without the second the browser throws on
// an undefined module.
//
// The alternative was a fourth file in a package whose whole argument is that it
// has three, or leaving the one place this page decides anything as the one
// place nothing can check. It was the second of those for as long as the page
// existed, and it cost a bug that told every visitor their deploy was refused.
if (typeof document !== "undefined") {
  wireSignIn();
  wireSignOut();
  wireTenant();
  start().catch(fail);
}

if (typeof module !== "undefined") {
  module.exports = { whatToShow, admissionKnown, backupView, short, when, newAppBody, newAppOutcome };
}
