// Connecting a repository, which is where `git push -> build -> deploy` starts.
//
// Every part of that chain existed and none of it was joined: placement.Store
// had SetTrigger and all three stores implemented it, the webhook handler
// resolved triggers and started builds, build_controller produced a digest —
// and nothing wrote a trigger, nothing read the digest, and no page offered
// any of it. This is the browser end.

function triggerUrl(base) {
  return `${base}/trigger`;
}

// The secret is write-only, everywhere, and this file is one of the places
// that has to hold that.
//
// The endpoint stores it in plaintext because an HMAC needs the key rather
// than a hash of it, and answers with a shape that has no field to return it
// in. The page must not undo that: the value is never read back, never
// remembered by the browser, and the field says what it is rather than
// pretending the stored one is there behind a mask.
const SECRET_NOTE =
  "stored to verify signatures and never shown again. Send a new one to replace it. " +
  "It is not in git and it is not in the page: this platform cannot show it to you " +
  "and neither can anyone who reaches this screen.";

// Three states, and the middle one is the one worth drawing.
//
//   unconfigured  no repository connected, so a push reaches nothing
//   connected     a repository is connected; the secret exists and is not shown
//   saving        in flight
//
// "connected" deliberately does not claim the webhook works. The platform
// knows what it was told, not whether the forge was configured to deliver —
// and a page that said "connected" about the forge's side would be asserting
// something it has never observed.
function sourceView(state) {
  switch (state.phase) {
    case "saving":
      return { state: "saving", text: "connecting…", ok: null };
    case "connected":
      return {
        state: "connected",
        text: state.repoUrl
          ? `${state.repoUrl} is connected — deliver pushes to ${state.deliverTo || "/hooks/github"}`
          : "connected",
        // Not a claim that a push has ever arrived. This is what was stored.
        ok: true,
        note: "damga has not seen a push yet unless one is listed below; this is what " +
          "was configured, not what has happened.",
      };
    case "refused":
      return { state: "refused", text: state.detail || "that could not be connected", ok: false };
    default:
      return {
        state: "unconfigured",
        text: "no repository is connected, so a push to one reaches nothing here",
        ok: null,
      };
  }
}

// The server's words for a refusal.
function refusalText(status, body) {
  const detail = body && typeof body.detail === "string" ? body.detail : "";
  if (detail) return detail;
  switch (status) {
    case 403:
      return "connecting a repository is owner-only, and this account is not an owner";
    case 404:
      return "this app and environment have no repository configured yet, so a push to " +
        "it would have nowhere to deploy";
    default:
      return `connecting was refused with ${status} and no detail`;
  }
}

// mountSource renders the view into el and returns a stop function.
function mountSource(el, base, options = {}) {
  const { fetcher = globalThis.fetch } = options;
  const doc = el.ownerDocument;
  let stopped = false;

  const node = (tag, attrs = {}, ...kids) => {
    const n = doc.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
    n.append(...kids.flat());
    return n;
  };

  const status = doc.createElement("p");
  status.className = "muted";
  const form = doc.createElement("form");
  form.className = "source-form";
  const errors = doc.createElement("ul");
  errors.className = "refusal";
  errors.hidden = true;

  const repo = doc.createElement("input");
  repo.type = "text";
  repo.placeholder = "https://github.com/you/app.git";
  repo.setAttribute("aria-label", "Repository URL");
  repo.setAttribute("autocomplete", "off");

  const secret = doc.createElement("input");
  // Not type=password: masking implies a stored value is behind the dots, and
  // there is never one here. The field is always empty and always new.
  secret.type = "text";
  secret.placeholder = "webhook secret (at least 16 characters)";
  secret.setAttribute("aria-label", "Webhook secret");
  secret.setAttribute("autocomplete", "off");

  const connect = doc.createElement("button");
  connect.type = "submit";
  connect.textContent = "Connect";

  const note = node("p", { class: "source-note" }, SECRET_NOTE);
  form.replaceChildren(repo, secret, connect, note);
  el.replaceChildren(status, form, errors);

  const paint = (state) => {
    const view = sourceView(state);
    status.textContent = view.text;
    status.dataset.ok = String(view.ok);
    status.dataset.state = view.state;
    errors.hidden = view.state !== "refused";
    if (view.state === "refused") {
      errors.replaceChildren(node("li", {}, view.text));
    }
  };

  paint({ phase: "unconfigured" });

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (stopped) return;
    connect.disabled = true;
    paint({ phase: "saving" });
    try {
      const response = await fetcher(triggerUrl(base), {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          provider: "github", repoUrl: repo.value, secret: secret.value,
        }),
      });
      const body = await response.json().catch(() => null);
      if (!response.ok) {
        paint({ phase: "refused", detail: refusalText(response.status, body) });
        return;
      }
      // Cleared immediately. The page has no reason to keep it for a second
      // longer than the request took, and a field left full is a field a
      // screenshot catches.
      secret.value = "";
      paint({
        phase: "connected",
        repoUrl: (body && body.repoUrl) || repo.value,
        deliverTo: body && body.deliverTo,
      });
    } catch (err) {
      paint({ phase: "refused", detail: String((err && err.message) || err) });
    } finally {
      connect.disabled = false;
    }
  });

  return function stop() {
    stopped = true;
  };
}

// The page's entry point:
//
//   index.html:  <script src="/source.js"></script>   before app.js
//   app.js:      window.damgaSource.mountSource(box, base(), {});
if (typeof window !== "undefined") {
  window.damgaSource = { mountSource, triggerUrl, sourceView, refusalText, SECRET_NOTE };
}

if (typeof module !== "undefined") {
  module.exports = { mountSource, triggerUrl, sourceView, refusalText, SECRET_NOTE };
}
