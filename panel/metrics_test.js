'use strict';

// The health view's decisions.
//
// One of them is the reason this file exists. The endpoint has three honest
// answers about CPU and memory — a value, no metrics-server in the cluster, and
// a metrics-server with no sample for these pods yet — and the third was found
// by running it against a crash-looping application, where every pod came back
// with no usage under a source that said it had answered. A unit test with a
// fake in it could not have found it; it can keep it.

const test = require('node:test');
const assert = require('node:assert');

const { usageView, statusView, restartView, sampleText, podRows } = require('./assets/metrics.js');

const pod = (extra = {}) => ({
  name: 'api-1', phase: 'Running', ready: true, restarts: 0,
  cpu: {}, memory: {}, ...extra,
});

// ------------------------------------------------------------ the three states

test('a value, a missing component and an empty answer are three different screens', () => {
  const live = usageView({
    usage: { source: 'metrics.k8s.io' },
    pods: [pod({ cpu: { usage: '1m' }, memory: { usage: '10Mi' } })],
  });
  const absent = usageView({
    usage: { note: 'live CPU and memory need metrics-server, and metrics.k8s.io is not answering' },
    pods: [pod()],
  });
  const nosample = usageView({
    usage: { source: 'metrics.k8s.io', note: 'metrics.k8s.io answered but has no sample for these pods yet' },
    pods: [pod()],
  });

  const states = [live.state, absent.state, nosample.state];
  assert.deepStrictEqual(states, ['live', 'absent', 'nosample'],
    'the three answers have to stay three; collapsing two of them is how the finding disappears');
  assert.strictEqual(new Set(states).size, 3);

  // The columns are the visible difference between the two that have no value.
  // With no component there is nothing to put under a heading called Memory;
  // with a component and no sample yet, the column stays so that it visibly
  // fills in on the next scrape.
  assert.strictEqual(live.columns, true);
  assert.strictEqual(absent.columns, false, 'an empty cell under "Memory" is still a claim about memory');
  assert.strictEqual(nosample.columns, true);
});

test('the component that is missing is named, in the server\'s own words', () => {
  const note = 'live CPU and memory need metrics-server, and metrics.k8s.io is not answering in this cluster';
  const view = usageView({ usage: { note }, pods: [pod()] });
  assert.strictEqual(view.text, note,
    'the panel decides nothing: the sentence that names the component is the API\'s');
  assert.match(view.text, /metrics-server/);
});

test('one pod with a value is enough to be live, and none is not', () => {
  const someMeasured = usageView({
    usage: { source: 'metrics.k8s.io' },
    pods: [pod({ name: 'api-1' }), pod({ name: 'api-2', memory: { usage: '10Mi' } })],
  });
  assert.strictEqual(someMeasured.state, 'live');

  const noneMeasured = usageView({ usage: { source: 'metrics.k8s.io' }, pods: [pod(), pod()] });
  assert.strictEqual(noneMeasured.state, 'nosample',
    'a source that answered and matched no pod is not the same as one that was asked for nothing');
});

// ------------------------------------------------------------ per replica

// The state that sends people to the logs of a container that is behaving
// perfectly: the process is up, the readiness probe is failing, and the
// Deployment is holding it out of the Service.
test('Running and not Ready is not reported as healthy', () => {
  assert.deepStrictEqual(statusView(pod({ ready: true })), { text: 'Running', ok: true });

  const notReady = statusView(pod({ ready: false }));
  assert.strictEqual(notReady.ok, false, 'a replica serving nothing was shown as fine');
  assert.match(notReady.text, /hazır değil/);
});

test('a replica that is not Running says which phase it is in', () => {
  const view = statusView(pod({ phase: 'Pending', ready: false }));
  assert.strictEqual(view.text, 'Pending');
  assert.strictEqual(view.ok, false);
});

// A count says something is wrong; the reason says what. OOMKilled and exit 1
// send the reader to two different places.
test('restarts carry the reason the last container ended', () => {
  assert.deepStrictEqual(restartView(pod({ restarts: 0 })), { text: 'yok', ok: true });

  const oom = restartView(pod({ restarts: 7, lastTerminated: { reason: 'OOMKilled', exitCode: 137 } }));
  assert.strictEqual(oom.ok, false);
  assert.match(oom.text, /7 yeniden başlatma/);
  assert.match(oom.text, /OOMKilled/, 'the count without the reason is the half nobody can act on');
  assert.match(oom.text, /137/);

  // A restart with nothing recorded about it still has to say the count.
  const bare = restartView(pod({ restarts: 2 }));
  assert.strictEqual(bare.text, '2 yeniden başlatma');
  assert.strictEqual(bare.ok, false);

  // Singular, because "1 restarts" is the kind of thing that makes a reader
  // wonder what else was not looked at.
  assert.match(restartView(pod({ restarts: 1 })).text, /^1 yeniden başlatma\b/);
});

// ------------------------------------------------------------ the numbers

test('a usage figure is always shown against what it is measured against', () => {
  const text = sampleText({ usage: '500Mi', request: '128Mi', limit: '512Mi', ofLimitPercent: 97 });
  assert.match(text, /500Mi/);
  assert.match(text, /512Mi/, '500Mi is fine against 2Gi and an outage against 512Mi');
  assert.match(text, /97%/);
});

// The percentage is the server's arithmetic. Recomputing it here is how a CLI
// and a panel come to disagree about the same pod.
test('the percentage is the one the server sent', () => {
  const text = sampleText({ usage: '500Mi', limit: '512Mi', ofLimitPercent: 3 });
  assert.match(text, /\(3%\)/, 'the panel recomputed a number the API had already answered');
});

test('a pod with no sample says so rather than showing a zero', () => {
  assert.strictEqual(sampleText({ request: '128Mi', limit: '512Mi' }, { columns: true }), 'henüz örnek yok');
  assert.strictEqual(sampleText(undefined, { columns: true }), '—');
});

test('with no limit the figure is still given something to stand against', () => {
  const text = sampleText({ usage: '12m', request: '100m' });
  assert.match(text, /12m/);
  assert.match(text, /100m/, 'a bare number is one nobody can act on');
  assert.doesNotMatch(text, /%/, 'there is no limit, so there is no percentage of one');
});

// ------------------------------------------------------------ the whole table

test('the rows carry one line per replica, whatever the usage state', () => {
  const response = {
    usage: { source: 'metrics.k8s.io', note: 'no sample for these pods yet' },
    pods: [
      pod({ name: 'api-1', ready: false, restarts: 6, lastTerminated: { reason: 'OOMKilled', exitCode: 137 } }),
      pod({ name: 'api-2' }),
    ],
  };
  const rows = podRows(response);
  assert.strictEqual(rows.length, 2);
  assert.strictEqual(rows[0].name, 'api-1');
  assert.strictEqual(rows[0].status.ok, false);
  assert.match(rows[0].restarts.text, /OOMKilled/);
  // The pod facts survive the usage being absent, which is the whole reason the
  // endpoint reads two sources that fail apart.
  assert.strictEqual(rows[1].status.ok, true);
  assert.strictEqual(rows[0].memory, 'henüz örnek yok');
});

test('an app with no replicas is not an error', () => {
  assert.deepStrictEqual(podRows({ usage: {}, pods: [] }), []);
});

// The one thing this file cannot do for itself. app.js owns the single place a
// 401 becomes the sign-in form, and a view that called fetch directly would
// render "HTTP 401" into a table instead — so the dependency is required rather
// than defaulted, and the failure is at the call site instead of on the page.
test('mounting without a fetcher fails at the call site', () => {
  const { mountMetrics } = require('./assets/metrics.js');
  assert.throws(() => mountMetrics({}, '/base', {}), /fetcher/);
});
