// The live log view: what the process is writing, as it writes it.
//
// The page could say what a deploy did and never what it said afterwards, which
// is the half somebody looks for when a deploy went green and the app is still
// wrong.
//
// Nothing here runs on load. app.js decides when an app is selected, and this
// file is a function it calls — see mountLogs. The two guards at the bottom are
// the ones app.js already carries: a browser has no module, node --test has no
// document, and this file has to be both.

// EventSource and not fetch with a reader.
//
// The framing, the reconnect and the backoff are the parts that would have to
// be written by hand, and they are the parts that are wrong in every hand
// written version — including a first draft of this one. What is written by
// hand here is the part EventSource does badly: it reconnects for ever, and a
// stream refused with 403 or 501 looks to it exactly like a network blip.
const MAX_LINES = 2000;
const MAX_RETRIES = 5;

// How long to wait before reconnecting, and when to stop trying.
//
// null means stop. EventSource on its own never stops: a stream the server
// refuses — an install that cannot read logs at all, a session that ended — is
// retried at its own pace until the tab is closed, which turns one broken page
// into a request every few seconds for as long as somebody leaves it open.
//
// Backoff doubles from a second, and the count resets whenever a line arrives:
// a stream that ran for an hour and dropped is a blip, not a failure, and it
// should not be one retry away from giving up.
function nextRetry(failures) {
  if (failures > MAX_RETRIES) return null;
  return Math.min(1000 * 2 ** (failures - 1), 30000);
}

// The URL of the stream. tail is sent explicitly rather than left to the
// server's default, because this is also the value the reader can see and
// change, and a page that shows "last 200 lines" while asking for whatever the
// server felt like is lying in a small way.
function logsUrl(base, { tail = 200, follow = true } = {}) {
  const query = new URLSearchParams({ tail: String(tail), follow: String(follow) });
  return `${base}/logs?${query}`;
}

// One line, as one string. The timestamp is local and the container is named
// only when there is more than one — a single-container app would otherwise
// repeat the same word down the left of every line it ever writes.
function lineText(event, { showContainer = true } = {}) {
  const at = event.at ? new Date(event.at) : null;
  const stamp = at && !Number.isNaN(at.valueOf()) ? at.toLocaleTimeString() : "";
  const who = showContainer && event.container ? `${event.pod}/${event.container}` : event.pod;
  return [stamp, who, event.text].filter(Boolean).join("  ");
}

// Appending with a ceiling, because a followed stream has no end and the DOM
// it fills has no ceiling of its own. The oldest lines go, which is the only
// choice that keeps the thing somebody is watching — the newest — on screen.
function appendLine(lines, event, max = MAX_LINES) {
  const next = lines.concat([event]);
  return next.length <= max ? next : next.slice(next.length - max);
}

// What the status line says. Which sentence is true is the whole decision, and
// building it out of DOM nodes would put it behind a browser — the same reason
// backupView in app.js returns values.
//
// "ended" and "failed" are separate on purpose. A stream that finished is not a
// stream that broke, and an app that is scaled to zero has finished: the server
// says so with a reason, and repeating that reason is more use than the word
// "disconnected".
function streamStatus(kind, detail = "") {
  switch (kind) {
    case "connecting":
      return { text: "bağlanılıyor…", ok: null };
    case "live":
      return { text: "canlı", ok: true };
    case "ended":
      return { text: detail || "akış sona erdi", ok: null };
    case "failed":
      return { text: detail || "akış durdu", ok: false };
    default:
      return { text: kind, ok: null };
  }
}

// mountLogs renders the view into el and returns a stop function.
//
// The stop function is not a convenience. Leaving an app for another one, or
// signing out, otherwise leaves an open connection per app somebody has looked
// at, all of them still holding a stream on the server.
function mountLogs(el, base, options = {}) {
  const { tail = 200, follow = true, max = MAX_LINES } = options;
  const doc = el.ownerDocument;

  const status = doc.createElement("p");
  status.className = "muted";
  const pre = doc.createElement("pre");
  pre.className = "logs";
  pre.setAttribute("aria-live", "polite");
  el.replaceChildren(status, pre);

  let lines = [];
  let failures = 0;
  let source = null;
  let timer = null;
  let stopped = false;

  const paint = (kind, detail) => {
    const state = streamStatus(kind, detail);
    status.textContent = state.text;
    status.dataset.ok = String(state.ok);
  };

  const draw = () => {
    // Pinned to the bottom only when the reader is already there. Scrolling
    // somebody back down while they are reading three screens up is the one
    // way a live view makes itself unusable.
    const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
    pre.textContent = lines.map((event) => lineText(event)).join("\n");
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  };

  const open = () => {
    if (stopped) return;
    paint("connecting");
    source = new EventSource(logsUrl(base, { tail, follow }));

    source.addEventListener("line", (message) => {
      failures = 0;
      lines = appendLine(lines, JSON.parse(message.data), max);
      paint("live");
      draw();
    });

    source.addEventListener("end", (message) => {
      const { reason } = JSON.parse(message.data);
      // Closed by hand. EventSource treats the server closing the connection
      // as a fault and reconnects, so a finished stream would restart for ever
      // — and a finished stream is the ordinary outcome of follow=false.
      source.close();
      paint("ended", reason);
    });

    source.addEventListener("error", (message) => {
      // Two different events arrive on this name: the server's own error frame,
      // which carries data, and EventSource's connection failure, which does
      // not. Only the second is worth a retry.
      if (message.data) {
        source.close();
        paint("failed", JSON.parse(message.data).detail);
        return;
      }
      source.close();
      failures += 1;
      const wait = nextRetry(failures);
      if (wait === null) {
        paint("failed", "akış durdu ve geri gelmedi");
        return;
      }
      paint("connecting");
      timer = setTimeout(open, wait);
    });
  };

  open();

  return function stop() {
    stopped = true;
    if (timer) clearTimeout(timer);
    if (source) source.close();
  };
}

// The page's entry point. app.js has no imports — the panel has no build step —
// so this is how it reaches this file:
//
//   index.html:  <script type="module" src="/logs.js"></script>   before app.js
//   app.js:      window.damgaLogs.mountLogs(box, base(), {});
//
// Neither line is in this file's gift; both are named in the report that came
// with it.
if (typeof window !== "undefined") {
  window.damgaLogs = { mountLogs, logsUrl, lineText, appendLine, streamStatus, nextRetry };
}

if (typeof module !== "undefined") {
  module.exports = { mountLogs, logsUrl, lineText, appendLine, streamStatus, nextRetry, MAX_LINES };
}
