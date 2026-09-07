// What the application itself is doing: is it up, is it being restarted, and
// is it running out of memory.
//
// The deploy record says what was shipped and the log says what the process
// wrote. Neither answers the question somebody actually arrives with — "it was
// fine yesterday and now it keeps dying" — and the three fields that do are
// here: readiness, restarts with the reason the last container ended, and
// memory against the limit it is measured against.
//
// Nothing here runs on load. app.js decides when an app is selected and calls
// mountMetrics; the two guards at the bottom are the ones logs.js already
// carries, because a browser has no module and node --test has no document.

// The panel decides nothing, and this file is where that rule earns its keep.
//
// The endpoint has three honest answers about CPU and memory and they are not
// interchangeable: a value, a cluster with no metrics-server in it, and a
// metrics-server that has no sample for these pods yet. The third is real —
// a container that is restarting faster than the scrape interval is never
// scraped — and it was found by running the endpoint against a crash-looping
// app, where every pod came back with no usage under a source that said it had
// answered. Rendered as "0" or as a blank cell, that finding disappears and the
// page says the application uses no memory.
//
// So the state is read off the response rather than inferred from the absence
// of a number, and each one gets a different screen: values, or per-pod "no
// sample yet" with the component still named, or no usage columns at all.
function usageView(response) {
  const usage = response.usage || {};
  const pods = response.pods || [];
  const measured = pods.some((p) => (p.cpu && p.cpu.usage) || (p.memory && p.memory.usage));

  if (!usage.source) {
    // No component. The columns are left out entirely rather than filled with
    // dashes: an empty cell under a heading called "Memory" is a claim about
    // memory, and this install cannot make one.
    return {
      state: "absent",
      columns: false,
      text: usage.note || "bu kurulumda canlı CPU ve bellek yok",
      ok: null,
    };
  }
  if (!measured) {
    // The component answered and had nothing for these pods. Kept apart from
    // the case above because the fix is different and so is the outlook: this
    // one may resolve itself on the next scrape, and the columns stay so that
    // it visibly does.
    return {
      state: "nosample",
      columns: true,
      text: usage.note || `${usage.source} bu pod'lar için henüz örnek taşımıyor`,
      ok: null,
    };
  }
  return { state: "live", columns: true, text: `canlı, kaynak: ${usage.source}`, ok: true };
}

// Whether this replica is serving, which is not the same question as whether it
// is running.
//
// Running and not Ready is the state that sends people to the logs of a
// container that is behaving perfectly: the process is up, the readiness probe
// is failing, and the Deployment is quietly holding it out of the Service. A
// page that printed only the phase would call that healthy.
function statusView(pod) {
  const phase = pod.phase || "unknown";
  if (phase !== "Running") {
    return { text: phase, ok: false };
  }
  return pod.ready
    ? { text: "Running", ok: true }
    : { text: "Running, hazır değil", ok: false };
}

// The restart story, with the reason attached.
//
// A count on its own says something is wrong and not what, and the reason is
// the half a platform user cannot get at from the outside — the pod is Running,
// the Deployment is Available, and the application has been dying every four
// minutes since the deploy. OOMKilled and "exit 1" send the reader to two
// different places.
function restartView(pod) {
  const count = pod.restarts || 0;
  const last = pod.lastTerminated;
  if (count === 0) {
    return { text: "yok", ok: true };
  }
  const times = `${count} yeniden başlatma`;
  if (!last) {
    return { text: times, ok: false };
  }
  const why = last.reason || `çıkış kodu ${last.exitCode}`;
  const code = last.reason && last.exitCode !== undefined ? ` (çıkış kodu ${last.exitCode})` : "";
  return { text: `${times} · son ${why}${code}`, ok: false };
}

// One resource as one string, and every part of it comes from the server.
//
// The percentage in particular is not recomputed here. The API sends
// ofLimitPercent, and a page that divided the two strings itself would be a
// second implementation of the same arithmetic — which is how a CLI and a panel
// come to disagree about the same pod, and the one people believe is the one
// with the nicer typography.
function sampleText(sample, { columns = true } = {}) {
  if (!sample) return "—";
  if (!sample.usage) {
    return columns ? "henüz örnek yok" : "—";
  }
  const against = sample.limit ? ` / ${sample.limit}` : sample.request ? ` / istenen ${sample.request}` : "";
  const percent = sample.ofLimitPercent === undefined || sample.limit === undefined
    ? ""
    : ` (${sample.ofLimitPercent}%)`;
  return `${sample.usage}${against}${percent}`;
}

// The rows the table draws, as values. Building them out of DOM nodes would put
// the only part worth testing behind a browser — the same reason backupView in
// app.js returns values.
function podRows(response) {
  const view = usageView(response);
  return (response.pods || []).map((pod) => ({
    name: pod.name,
    status: statusView(pod),
    restarts: restartView(pod),
    cpu: sampleText(pod.cpu, view),
    memory: sampleText(pod.memory, view),
  }));
}

// mountMetrics renders the view into el and returns a stop function.
//
// A snapshot with a refresh, and not a poll. The endpoint says of itself that
// it is "the values as of this request, not a series over time", and a page
// that quietly re-fetched every few seconds would be building a series out of
// it — badly, one tab at a time, against a control plane that makes two cluster
// reads per call. What the reader gets instead is the time it was taken, which
// is the honest version of the same information.
function mountMetrics(el, base, options = {}) {
  // fetcher is required and has no default, which is deliberate. The obvious
  // default is fetch, and fetch is wrong twice over: it resolves to a Response
  // rather than to the parsed body, and it knows nothing about a session that
  // ended — app.js has one place that turns a 401 into the sign-in form, and a
  // second caller going around it is a page that renders "HTTP 401" in a table.
  // Defaulting would make both failures quiet; this one is loud and happens at
  // the call site.
  const { fetcher, now = () => new Date() } = options;
  if (typeof fetcher !== "function") {
    throw new TypeError("mountMetrics needs a fetcher: app.js passes its api()");
  }
  const doc = el.ownerDocument;
  let stopped = false;

  const heading = doc.createElement("h3");
  heading.textContent = "Sağlık";
  const status = doc.createElement("p");
  status.className = "muted";
  const body = doc.createElement("div");
  const foot = doc.createElement("p");
  foot.className = "muted footnote-limits";

  const refresh = doc.createElement("button");
  refresh.className = "link";
  refresh.type = "button";
  refresh.textContent = "Yenile";

  el.replaceChildren(heading, status, body, foot, refresh);

  const draw = (response) => {
    const view = usageView(response);
    status.textContent = `${view.text} · ${now().toLocaleTimeString()} itibarıyla`;
    status.dataset.state = view.state;

    const rows = podRows(response);
    if (rows.length === 0) {
      // No replicas at all is its own answer, and it is not an error: an app
      // scaled to zero, or one whose deploy has not been applied yet, has none.
      body.replaceChildren(muted(doc, "Çalışan replika yok."));
    } else {
      const headers = ["Replika", "Durum", "Yeniden başlatma"];
      if (view.columns) headers.push("CPU", "Bellek");
      body.replaceChildren(table(doc, headers, rows.map((row) => {
        const cells = [text(doc, row.name, "mono"), pill(doc, row.status), pill(doc, row.restarts)];
        if (view.columns) cells.push(text(doc, row.cpu), text(doc, row.memory));
        return cells;
      })));
    }

    // What the endpoint says it does not answer, shown rather than left to be
    // inferred from a missing chart. A panel that drew request rate from a
    // field that is simply absent would draw a flat line at zero.
    foot.replaceChildren(...(response.limits || []).map((line) => {
      const p = doc.createElement("span");
      p.className = "limit";
      p.textContent = line;
      return p;
    }));
  };

  const load = async () => {
    if (stopped) return;
    refresh.disabled = true;
    status.textContent = "yükleniyor…";
    delete status.dataset.state;
    try {
      const response = await fetcher(`${base}/metrics`);
      if (!stopped) draw(response);
    } catch (err) {
      if (stopped) return;
      // Said as what it is. This endpoint answers 501 on an install whose
      // control plane is not in a cluster, and "could not load" would send the
      // reader looking for a network problem that is not there.
      status.textContent = err && err.message ? err.message : "bu uygulamanın sağlığı okunamadı";
      status.dataset.state = "failed";
      body.replaceChildren();
      foot.replaceChildren();
    } finally {
      if (!stopped) refresh.disabled = false;
    }
  };

  refresh.addEventListener("click", load);
  load();

  return function stop() {
    stopped = true;
  };
}

// Small DOM helpers, local to this file. app.js has its own and does not export
// them; the panel has no build step, so a shared module would be a fourth
// script tag and a load-order rule for two functions.
function text(doc, value, className) {
  const node = doc.createElement("span");
  if (className) node.className = className;
  node.textContent = value;
  return node;
}

function muted(doc, value) {
  return text(doc, value, "muted");
}

// ok true and false get the page's two pill colours; null stays muted text,
// which is the vocabulary the evidence view already uses for "neither a success
// nor a failure".
function pill(doc, view) {
  if (view.ok === null || view.ok === undefined) return muted(doc, view.text);
  return text(doc, view.text, `pill ${view.ok ? "ok" : "bad"}`);
}

function table(doc, headers, rows) {
  const thead = doc.createElement("thead");
  const hrow = doc.createElement("tr");
  for (const h of headers) {
    const th = doc.createElement("th");
    th.textContent = h;
    hrow.append(th);
  }
  thead.append(hrow);

  const tbody = doc.createElement("tbody");
  for (const cells of rows) {
    const tr = doc.createElement("tr");
    for (const cell of cells) {
      const td = doc.createElement("td");
      td.append(cell);
      tr.append(td);
    }
    tbody.append(tr);
  }
  const node = doc.createElement("table");
  node.append(thead, tbody);
  return node;
}

if (typeof window !== "undefined") {
  window.damgaMetrics = { mountMetrics, usageView, statusView, restartView, sampleText, podRows };
}

if (typeof module !== "undefined") {
  module.exports = { mountMetrics, usageView, statusView, restartView, sampleText, podRows };
}
