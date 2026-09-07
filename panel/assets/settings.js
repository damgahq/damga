// The application's settings: environment variables, health, resource limits.
//
// The gap this closes is the most visible one the product has. n8n does not
// install from the catalogue because its template shares a token across two
// variables and nobody can type a value; in Coolify that is a text box and
// here it was a refusal. docs/PLAN.md §6 has the whole reading.
//
// Nothing here runs on load. app.js calls mountSettings; the guards at the
// bottom are the ones every view in this directory carries.

// One place that knows the wire shape.
//
// The endpoint is being written on another branch as this is written, so the
// shape below is the one proposed to its author and not yet confirmed by a
// commit. It is isolated here on purpose: when the contract is settled, a
// correction is an edit to normalise and nothing above or below it moves.
// The alternative — reading response fields wherever they happen to be needed
// — is how the two halves of the from-compose label came to be separately
// green and never met.
function normalise(response) {
  const body = response || {};
  return {
    env: (body.env || []).map((entry) => ({
      key: entry.key,
      // Absent and "" are the same thing for a literal, and that is the CRD's
      // shape rather than a convenience: EnvVar is {name, value}, so there is
      // no object that means "declared with no value". An earlier draft of
      // this file drew that as a fourth state and would have shown a
      // distinction the data cannot carry.
      value: entry.secret ? undefined : (entry.value === undefined ? "" : entry.value),
      secret: Boolean(entry.secret),
      build: Boolean(entry.build),
      runtime: Boolean(entry.runtime),
      // Which Secret this variable reads, which is the thing git shows and
      // Coolify does not show at all. Named <app>-env by the endpoint.
      secretRef: entry.secretRef || null,
    })),
    health: body.health || {},
    resources: body.resources || {},
    // The server's own sentences. A build-time variable is recorded and
    // consumed by nothing today — BuildSpec has no Env field — and that is the
    // endpoint's finding to report, not this page's to infer.
    warnings: body.warnings || [],
    secretNote: body.secretNote || "",
  };
}

// What a single variable is: three screens over two wire states.
//
// The wire has two — a literal value, or a secret whose value is never sent.
// The screen has three, because a literal that is the empty string draws as an
// empty box and an empty box reads as "nothing here". "" is a real setting: an
// application reads it as present-and-empty. So it says so in words rather
// than being rendered as the absence of a value.
//
// That is metrics.js's rule pointed the right way round. The rule is not "more
// states are better" — an earlier draft of this file invented a fourth state
// for "declared with no value", which the CRD cannot represent, and would have
// drawn a difference that does not exist.
//
//   plain   a value, in git, shown
//   empty   the empty string — in git, and a real setting
//   secret  a value that exists, is not shown, and is not in git
function envRowView(entry, secretNote = "") {
  if (entry.secret) {
    return {
      state: "secret",
      // Never rendered into the value box. The endpoint cannot send it — the
      // control plane holds create and patch on Secrets and deliberately not
      // get — so there is nothing here to show even by mistake.
      shown: false,
      text: "ayarlı — gösterilmiyor, ve git'te değil",
      inGit: false,
      // The server's sentence when it sent one. It carries the 2026-09-04
      // decision and the 2026-09-02 measurement behind it, and printing the
      // endpoint's words rather than a second copy is the same rule the
      // catalogue screen is under.
      warning: secretNote || SECRET_NOTE_FALLBACK,
      ok: null,
    };
  }
  if (entry.value === "") {
    return {
      state: "empty",
      shown: true,
      text: "boş dizeye ayarlı",
      inGit: true,
      warning: "",
      ok: null,
    };
  }
  return { state: "plain", shown: true, text: entry.value, inGit: true, warning: "", ok: true };
}

// Used only when the response carried no secretNote. Kept short and pointing
// at the same two facts the endpoint's longer sentence carries, so a page
// talking to an older control plane still says the thing that matters.
const SECRET_NOTE_FALLBACK =
  "git'te değil: bu değer geri alınamaz, ve Secret silinirse hiçbir şey onu " +
  "geri koymaz";

// Where a variable applies, as two independent facts.
//
// PLAN §6 is explicit that this was Coolify's design decision and a correct
// one: is_buildtime and is_runtime are two booleans and not two values of one
// flag, because a variable can be both and can be neither. Neither is the case
// worth naming — a variable that applies nowhere is almost always a mistake,
// and it is the one this sentence exists to make visible.
function scopeText(entry) {
  if (entry.build && entry.runtime) return "derleme ve çalışma zamanı";
  if (entry.build) return "yalnızca derleme";
  if (entry.runtime) return "yalnızca çalışma zamanı";
  return "ne derleme ne çalışma zamanı — bu değişken hiçbir yere uygulanmıyor";
}

// Whether a scope is a mistake worth marking. Kept apart from the sentence so
// the caller styles it without parsing prose.
function scopeIsUseless(entry) {
  return !entry.build && !entry.runtime;
}

// The body a save sends. A full replace of the env list.
//
// Deletion is omission — a variable that is not in the list is removed, and
// that is the only way to remove one, for a secret as much as for a literal.
// There is no "clear" verb: the endpoint answers 400 to a secret carrying an
// empty value, because "a secret with an empty value is not a secret", and
// that refusal is deliberately the server's rather than a shape this page can
// express by accident.
//
// The dangerous case is a secret the reader did not retype. Its value is not
// in git, so a save that dropped it would destroy something no revert can
// bring back. The rule is that a secret row contributes no `value` at all
// unless something was typed into it — the absence of the field is the
// instruction to keep what is there. The endpoint now also refuses the
// destructive shape outright, so this is the near side of two locks rather
// than the only one.
function settingsBody(rows, extras = {}) {
  const env = rows.map((row) => {
    const entry = {
      key: row.key,
      build: Boolean(row.build),
      runtime: Boolean(row.runtime),
      secret: Boolean(row.secret),
    };
    if (row.secret && !row.typed) return entry;
    entry.value = row.value === undefined ? "" : row.value;
    return entry;
  });
  const body = { env };
  if (extras.health) body.health = extras.health;
  if (extras.resources) body.resources = extras.resources;
  return body;
}

// What a refusal says, in the server's words.
//
// One string, not a list. The catalogue's endpoint answers with several
// because three separate limits can stop an install at once; here the first
// bad field is the answer, so a page that drew a list would be drawing an
// array of one for ever.
function refusalText(status, body) {
  const detail = body && typeof body.detail === "string" ? body.detail : "";
  if (detail) return detail;
  switch (status) {
    case 403:
      return "bu hesabın ayarları değiştirme izni yok";
    case 404:
      return "bu uygulama ve ortam için henüz bir depo tanımlı değil";
    default:
      return `ayarlar ${status} ile ve hiçbir ayrıntı vermeden reddedildi`;
  }
}

// What the page says after a save, and the two answers are not the same event.
//
// A settings change is a commit — the thing this product has that Coolify's
// Save button does not: who changed it, when, and a way back. So the
// confirmation names the commit.
//
// Except when nothing was committed. A PUT that changed only secret VALUES
// answers 200 with a null record, because git never carries those values and
// there was nothing to write. Saying "saved" alone would be true and useless;
// saying "committed" would be a lie about a change that really happened. It
// says which of the two this was.
function savedText(response) {
  const body = response || {};
  const record = body.record || null;
  if (record) {
    const sha = record.commit || record.sha || record.source?.commit || "";
    return sha ? `${String(sha).slice(0, 7)} olarak commit edildi` : "commit edildi";
  }
  return "kaydedildi — hiçbir şey commit edilmedi, çünkü git sır değerlerini taşımıyor";
}

// The server's own warnings, printed rather than interpreted.
//
// It reports things this page cannot know: a variable marked build-time when
// BuildSpec carries no Env field and the build would consume nothing, or one
// marked neither build nor runtime and delivered nowhere. Both are cases where
// a setting is recorded and does nothing, which is the failure this whole page
// exists to stop being invisible.
function warningsOf(response) {
  const body = response || {};
  return Array.isArray(body.warnings) ? body.warnings : [];
}

// settingsUrl is the one address this view uses.
function settingsUrl(base) {
  return `${base}/settings`;
}

// mountSettings renders the view into el and returns a stop function.
function mountSettings(el, base, options = {}) {
  const { fetcher = globalThis.fetch } = options;
  const doc = el.ownerDocument;
  let stopped = false;
  let rows = [];
  let secretNote = "";

  const status = doc.createElement("p");
  status.className = "muted";
  const table = doc.createElement("div");
  table.className = "settings-env";
  const actions = doc.createElement("div");
  actions.className = "settings-actions";
  const save = doc.createElement("button");
  save.type = "button";
  save.textContent = "Kaydet";
  const add = doc.createElement("button");
  add.type = "button";
  add.className = "link";
  add.textContent = "Değişken ekle";
  actions.replaceChildren(add, save);
  const errors = doc.createElement("ul");
  errors.className = "refusal";
  errors.hidden = true;
  // The server's warnings, kept apart from its refusals. A warning is a save
  // that happened and did less than it looks like; a refusal is a save that
  // did not happen. Drawing them in one list would make the first read as the
  // second.
  const notes = doc.createElement("ul");
  notes.className = "settings-warnings";
  notes.hidden = true;
  el.replaceChildren(status, table, actions, notes, errors);

  const node = (tag, attrs = {}, ...kids) => {
    const n = doc.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
    n.append(...kids.flat());
    return n;
  };

  const showRefusal = (text) => {
    errors.replaceChildren(...(text ? [node("li", {}, text)] : []));
    errors.hidden = !text;
  };

  const showWarnings = (list) => {
    notes.replaceChildren(...list.map((text) => node("li", {}, text)));
    notes.hidden = list.length === 0;
  };

  const draw = () => {
    table.replaceChildren(
      ...rows.map((row, index) => {
        const view = envRowView(row, secretNote);
        const line = node("div", { class: "settings-row", "data-state": view.state });

        const key = doc.createElement("input");
        key.type = "text";
        key.value = row.key || "";
        key.placeholder = "AD";
        key.setAttribute("aria-label", "Değişken adı");
        key.setAttribute("autocomplete", "off");
        key.addEventListener("input", () => { rows[index].key = key.value; });

        const value = doc.createElement("input");
        // Not type=password. Masking implies the value is there and hidden,
        // and it is not there at all — the endpoint never sent it. The
        // placeholder says what is true instead.
        value.type = "text";
        value.value = view.shown && view.state === "plain" ? row.value : "";
        value.placeholder = view.state === "secret" ? "korumak için boş bırakın" : view.text;
        value.setAttribute("aria-label", "Değer");
        // The rule this page inherits from exec: what somebody types here is
        // never stored by the browser. A remembered value undoes the reason
        // the server refuses to log one.
        value.setAttribute("autocomplete", "off");
        value.addEventListener("input", () => {
          rows[index].value = value.value;
          rows[index].typed = true;
        });

        const build = doc.createElement("input");
        build.type = "checkbox";
        build.checked = Boolean(row.build);
        build.setAttribute("aria-label", "Derleme zamanında geçerli");
        build.addEventListener("change", () => { rows[index].build = build.checked; draw(); });

        const runtime = doc.createElement("input");
        runtime.type = "checkbox";
        runtime.checked = Boolean(row.runtime);
        runtime.setAttribute("aria-label", "Çalışma zamanında geçerli");
        runtime.addEventListener("change", () => { rows[index].runtime = runtime.checked; draw(); });

        const scope = node("span", { class: "settings-scope" }, scopeText(row));
        if (scopeIsUseless(row)) scope.dataset.ok = "false";

        // Removal is how a variable is deleted: the save sends a full list
        // and what is not in it is gone. Said on the button, because "Remove"
        // alone does not tell somebody that a secret's value goes with it and
        // does not come back.
        const remove = doc.createElement("button");
        remove.type = "button";
        remove.className = "link";
        remove.textContent = "Kaldır";
        remove.title = row.secret
          ? "Değişkeni ve değerini Secret'tan kaldırır. Değer git'te değil."
          : "Değişkeni bir sonraki kayıtta kaldırır.";
        remove.addEventListener("click", () => {
          rows = rows.filter((_, i) => i !== index);
          draw();
        });

        line.append(
          key, value,
          node("label", {}, build, "derleme"),
          node("label", {}, runtime, "çalışma zamanı"),
          scope, remove,
        );
        if (view.warning) {
          line.append(node("p", { class: "settings-warning" }, view.warning));
        }
        return line;
      }),
    );
  };

  add.addEventListener("click", () => {
    rows = rows.concat([{ key: "", value: "", build: false, runtime: true, secret: false, typed: true }]);
    draw();
  });

  save.addEventListener("click", async () => {
    if (stopped) return;
    save.disabled = true;
    showRefusal("");
    status.textContent = "kaydediliyor…";
    status.dataset.ok = "null";
    try {
      const response = await fetcher(settingsUrl(base), {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(settingsBody(rows)),
      });
      const body = await response.json().catch(() => null);
      if (!response.ok) {
        showRefusal(refusalText(response.status, body));
        status.textContent = "kaydedilmedi";
        status.dataset.ok = "false";
        return;
      }
      status.textContent = savedText(body);
      status.dataset.ok = "true";
      showWarnings(warningsOf(body));
      // Redrawn from what came back rather than from what was sent. The
      // response carries the settings as they now are, and a page that kept
      // showing the form it submitted would hide anything the endpoint
      // normalised or refused to record.
      if (body && body.settings) {
        const fresh = normalise(body.settings);
        rows = fresh.env;
        secretNote = fresh.secretNote || secretNote;
        draw();
      } else {
        await load();
      }
    } catch (err) {
      showRefusal(String((err && err.message) || err));
      status.textContent = "kaydedilmedi";
      status.dataset.ok = "false";
    } finally {
      save.disabled = false;
    }
  });

  async function load() {
    status.textContent = "yükleniyor…";
    status.dataset.ok = "null";
    try {
      const response = await fetcher(settingsUrl(base), { method: "GET" });
      const body = await response.json().catch(() => null);
      if (!response.ok) {
        showRefusal(refusalText(response.status, body));
        status.textContent = "bu ayarlar okunamadı";
        status.dataset.ok = "false";
        return;
      }
      const read = normalise(body);
      rows = read.env;
      secretNote = read.secretNote;
      showWarnings(read.warnings);
      status.textContent = rows.length
        ? `${rows.length} değişken`
        : "ayarlı değişken yok";
      status.dataset.ok = "true";
      draw();
    } catch (err) {
      showRefusal(String((err && err.message) || err));
      status.textContent = "bu ayarlar okunamadı";
      status.dataset.ok = "false";
    }
  }

  load();

  return function stop() {
    stopped = true;
  };
}

// The page's entry point:
//
//   index.html:  <script src="/settings.js"></script>   before app.js
//   app.js:      window.damgaSettings.mountSettings(box, base(), {});
if (typeof window !== "undefined") {
  window.damgaSettings = {
    mountSettings, settingsUrl, normalise, envRowView, scopeText, scopeIsUseless,
    settingsBody, refusalText, savedText, warningsOf,
  };
}

if (typeof module !== "undefined") {
  module.exports = {
    mountSettings, settingsUrl, normalise, envRowView, scopeText, scopeIsUseless,
    settingsBody, refusalText, savedText, warningsOf,
  };
}
