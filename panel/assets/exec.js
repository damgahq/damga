// Running one command in a container, and reading what it wrote.
//
// The half of exec that a person can reach. The endpoint landed without it, and
// an API-only exec closes none of the gap it was built to close: "get a
// kubeconfig" is the answer it was supposed to replace.
//
// Nothing here runs on load. app.js decides when an app is selected and calls
// mountExec; the two guards at the bottom are the ones every view in this
// directory carries.

// fetch and a reader, and not EventSource — which is the opposite of the
// decision logs.js made, for two reasons that both point this way.
//
// The small one: EventSource only issues GET, and this endpoint is a POST
// because the command is a body rather than a query.
//
// The large one: EventSource reconnects, and reconnecting here would re-run the
// command. logs.js retries five times with backoff because a dropped log stream
// costs nothing to reopen — the lines are still there. A dropped exec stream is
// a command that has already run, possibly a migration, and asking for it again
// is asking for it twice. So this connects once, and when the connection breaks
// it says so instead of trying to be helpful.
const MAX_OUTPUT = 200000;

// The states this view draws, kept apart on purpose.
//
// metrics.js sets the standard this follows: three honest answers about memory
// are three screens, and an empty cell under a heading called "Memory" is a
// claim about memory. The same applies here and the trap is sharper, because
// the failure that looks like nothing is the expensive one.
//
//   idle     nothing has been run
//   running  in flight
//   exited   it ran and returned a code — 0 or not, this is a result
//   refused  it could not be run, and the server said why
//   lost     the connection broke while it was running
//
// "exited" and "refused" are the two that get folded together by every simpler
// version of this file, and folding them is wrong in both directions: a
// migration that exits 1 ran and printed why, and a command refused because
// the pod is crash-looping never touched anything.
//
// "lost" exists because of what it must not look like. The output pane going
// quiet is exactly what a command that printed nothing looks like, and the
// difference is that this one may have changed the database. ingress-nginx
// closes a stream silent for 60 seconds; the server heartbeats inside that, so
// silence here is a real break rather than a slow command.
function execView(state) {
  switch (state.phase) {
    case "idle":
      return { state: "idle", text: "henüz hiçbir şey çalıştırılmadı", ok: null, ran: false };
    case "running":
      return { state: "running", text: "çalışıyor…", ok: null, ran: true };
    case "exited": {
      const code = state.code;
      return {
        state: "exited",
        // The server's number, said plainly. A non-zero exit is the command's
        // answer and not a platform failure, so it does not get the word
        // "failed" — it gets its code.
        text: code === 0 ? "komut 0 ile çıktı" : `komut ${code} ile çıktı`,
        ok: code === 0,
        ran: true,
        code,
      };
    }
    case "refused":
      return {
        // The server's own sentence, unedited. It names which of five refusals
        // this was — nothing deployed, no pod that can take a command (and
        // which state each pod is in), more than one container, no such
        // container, no command — and the panel has nothing to add to any of
        // them. Rewriting them here would put the reason behind a second
        // vocabulary that has to be kept in step with the first.
        state: "refused",
        text: state.detail || "komut çalıştırılamadı",
        ok: false,
        ran: false,
      };
    case "lost":
      return {
        state: "lost",
        text:
          state.detail ||
          "komut çalışırken bağlantı koptu. Bitmiş olabilir; çıktısı burada " +
          "kesiliyor.",
        ok: false,
        // The point of this whole state. Something ran, and what it did is not
        // on this screen.
        ran: true,
      };
    default:
      return { state: state.phase, text: state.phase, ok: null, ran: false };
  }
}

// What a refusal before the stream says.
//
// These arrive as a status and a body rather than as an event, because they
// happen before the first byte: not signed in, not the owner, no such app, an
// install with no cluster to reach. The server's detail is printed whenever
// there is one — catalog.js does the same, and for the same reason.
function refusalText(status, body) {
  const detail = body && typeof body.detail === "string" ? body.detail : "";
  if (detail) return detail;
  switch (status) {
    case 403:
      return "komut çalıştırmak yalnızca sahibe açıktır, ve bu hesap sahip değil";
    case 501:
      return "bu kurulum kümede komut çalıştıramıyor";
    default:
      return `komut ${status} ile ve hiçbir ayrıntı vermeden reddedildi`;
  }
}

// Splitting what somebody typed into the argv the endpoint takes.
//
// Quote-aware, because the first thing anyone types is `psql -c "select 1"` and
// splitting that on spaces sends four arguments where two were meant. An
// unbalanced quote is refused by name rather than guessed at: closing it
// silently would run a command the reader did not write.
//
// What this deliberately does not do is wrap the input in `sh -c`. That would
// make every command depend on the image having a shell, which a distroless
// image does not — and the failure would arrive as the kubelet complaining
// about `sh`, naming neither the image nor the assumption.
function parseCommand(text) {
  const argv = [];
  let current = "";
  let quote = null;
  let has = false;
  for (const ch of String(text)) {
    if (quote) {
      if (ch === quote) quote = null;
      else current += ch;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      has = true;
      continue;
    }
    if (/\s/.test(ch)) {
      if (has || current) argv.push(current);
      current = "";
      has = false;
      continue;
    }
    current += ch;
  }
  if (quote) throw new Error(`komutta kapatılmamış bir ${quote} tırnağı var`);
  if (has || current) argv.push(current);
  return argv;
}

// SSE framing, by hand, because fetch hands back bytes and not events.
//
// This is the part logs.js declined to write and was right to decline: framing
// a stream by hand is where hand-written stream readers go wrong. It is here
// because EventSource cannot POST, so the choice was this or a GET that carries
// a command in its query string — where it would land in every access log
// between the browser and the pod, which is the one place the server has
// already gone out of its way not to put it.
//
// Returns the complete events and whatever is left over, because a chunk
// boundary falls wherever TCP decides and is nearly never a frame boundary.
// A line beginning with ':' is a comment — the server's heartbeat — and is
// dropped here rather than surfaced, so that a slow command does not look like
// a talkative one.
function parseFrames(buffer) {
  const events = [];
  let rest = String(buffer);
  let at;
  while ((at = rest.indexOf("\n\n")) !== -1) {
    const frame = rest.slice(0, at);
    rest = rest.slice(at + 2);
    let name = "message";
    const data = [];
    for (const line of frame.split("\n")) {
      if (line.startsWith(":")) continue;
      if (line.startsWith("event:")) name = line.slice(6).trim();
      else if (line.startsWith("data:")) data.push(line.slice(5).trim());
    }
    if (!data.length) continue;
    let payload;
    try {
      payload = JSON.parse(data.join("\n"));
    } catch {
      // A frame this file cannot read is reported rather than dropped. Dropping
      // it would take output off the screen and leave the reader believing the
      // command printed less than it did.
      events.push({ event: "stderr", data: { text: data.join("\n") } });
      continue;
    }
    events.push({ event: name, data: payload });
  }
  return { events, rest };
}

// Appending with a ceiling, for the reason logs.js has one: a command is
// entitled to print more than a page can hold.
function appendOutput(text, chunk, max = MAX_OUTPUT) {
  const next = text + chunk;
  return next.length <= max ? next : next.slice(next.length - max);
}

// mountExec renders the view into el and returns a stop function.
//
// stop abandons the reader. It does not and cannot stop the command: it is
// running in a container on the other side of an API server, and the honest
// thing is to say the output was abandoned rather than to imply it was
// cancelled.
function mountExec(el, base, options = {}) {
  const { fetcher = globalThis.fetch, max = MAX_OUTPUT } = options;
  const doc = el.ownerDocument;

  const form = doc.createElement("form");
  form.className = "exec-form";

  const input = doc.createElement("input");
  input.type = "text";
  input.name = "command";
  input.id = "exec-command";
  input.placeholder = "rails db:migrate";
  // No autocomplete and no history, deliberately, and the same rule the server
  // applies to its log: a command line is where people put a password. The
  // server records the program and never the arguments; a browser that
  // remembers this field would undo that by keeping the arguments on disk, on
  // the reader's machine, for the next person at it.
  input.setAttribute("autocomplete", "off");

  const container = doc.createElement("input");
  container.type = "text";
  container.name = "container";
  container.id = "exec-container";
  container.placeholder = "konteyner (yalnızca pod'da birden fazla varsa)";
  container.setAttribute("autocomplete", "off");

  const run = doc.createElement("button");
  run.type = "submit";
  run.textContent = "Çalıştır";

  const status = doc.createElement("p");
  status.className = "muted";
  const pre = doc.createElement("pre");
  pre.className = "exec-output";
  pre.setAttribute("aria-live", "polite");

  form.replaceChildren(input, container, run);
  el.replaceChildren(form, status, pre);

  let output = "";
  let abort = null;
  let stopped = false;

  const paint = (state) => {
    const view = execView(state);
    status.textContent = view.text;
    status.dataset.ok = String(view.ok);
    status.dataset.state = view.state;
    // Marked on the pane as well, so the stylesheet can make "the output stops
    // here" look unlike a command that simply printed nothing.
    pre.dataset.state = view.state;
  };

  const draw = () => {
    pre.textContent = output;
    pre.scrollTop = pre.scrollHeight;
  };

  paint({ phase: "idle" });

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (stopped || abort) return;

    let argv;
    try {
      argv = parseCommand(input.value);
    } catch (err) {
      paint({ phase: "refused", detail: err.message });
      return;
    }
    if (!argv.length) {
      paint({ phase: "refused", detail: "önce bir komut yazın" });
      return;
    }

    output = "";
    draw();
    paint({ phase: "running" });
    run.disabled = true;
    abort = new AbortController();

    try {
      const response = await fetcher(execUrl(base), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(
          container.value ? { command: argv, container: container.value } : { command: argv },
        ),
        signal: abort.signal,
      });

      if (!response.ok) {
        let body = null;
        try {
          body = await response.json();
        } catch {
          body = null;
        }
        paint({ phase: "refused", detail: refusalText(response.status, body) });
        return;
      }

      const finished = await readStream(response, {
        onText: (text) => {
          output = appendOutput(output, text, max);
          draw();
        },
      });
      paint(finished);
    } catch (err) {
      if (stopped) return;
      // Everything that reaches here is a broken connection rather than a
      // refusal: the request was accepted, so the command is running or has
      // run. This is the state that must not read as silence.
      paint({ phase: "lost" });
    } finally {
      run.disabled = false;
      abort = null;
    }
  });

  return function stop() {
    stopped = true;
    if (abort) abort.abort();
  };
}

// readStream turns the body into the terminal state of one run.
//
// It returns rather than throws for the outcomes the server described, so that
// the caller does not have to tell an exception carrying a server sentence
// apart from one carrying a socket error.
async function readStream(response, { onText }) {
  const reader = response.body.getReader();
  const decode = new TextDecoder();
  let buffer = "";
  let ended = null;

  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decode.decode(value, { stream: true });
    const { events, rest } = parseFrames(buffer);
    buffer = rest;
    for (const frame of events) {
      switch (frame.event) {
        case "stdout":
        case "stderr":
          onText(frame.data.text || "");
          break;
        case "exit":
          ended = { phase: "exited", code: frame.data.code };
          break;
        case "error":
          ended = { phase: "refused", detail: frame.data.message };
          break;
        default:
          break;
      }
    }
  }
  // The stream finished without saying how. The command was accepted, so
  // something ran and this page does not know what it returned.
  return ended || { phase: "lost" };
}

function execUrl(base) {
  return `${base}/exec`;
}

// The page's entry point. app.js has no imports — the panel has no build step —
// so this is how it reaches this file:
//
//   index.html:  <script src="/exec.js"></script>   before app.js
//   app.js:      window.damgaExec.mountExec(box, base(), {});
if (typeof window !== "undefined") {
  window.damgaExec = { mountExec, execUrl, execView, refusalText, parseCommand, parseFrames, appendOutput };
}

if (typeof module !== "undefined") {
  module.exports = {
    mountExec, execUrl, execView, refusalText, parseCommand, parseFrames, appendOutput, MAX_OUTPUT,
  };
}
