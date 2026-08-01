// Short, steady-traffic script used by `make chaos` (loadtest/chaos.sh),
// which kills and restarts the gateway process partway through this run.
// Not meant to be run standalone in the normal sense -- see chaos.sh.
//
// This deliberately does NOT gate on a raw request-level success rate.
// A first version did, and it was the wrong metric: while the gateway
// process is down, a connection-refused failure returns in microseconds
// while a real success takes ~200ms (the mock provider's configured
// latency), so a handful of VUs looping with no artificial delay fire
// off a hugely disproportionate number of failed attempts during even a
// sub-second outage, making the aggregate success rate look far worse
// than the actual downtime. What actually matters for this test -- does
// the gateway recover, and does every request either succeed or fail
// cleanly rather than hang -- is what's checked below instead.

import http from 'k6/http';
import { check } from 'k6';

const BASE_URL = __ENV.GATEWAY_URL || 'http://127.0.0.1:18080';
const TOKEN = __ENV.LOADTEST_TOKEN || 'sk-vk-loadtest';
const CHAT_URL = `${BASE_URL}/v1/chat/completions`;

export const options = {
  scenarios: {
    chaos: { executor: 'constant-vus', vus: 5, duration: '20s' },
  },
};

export default function () {
  const res = http.post(
    CHAT_URL,
    JSON.stringify({
      model: 'fast',
      temperature: 0.7,
      messages: [{ role: 'user', content: `chaos probe ${__VU}-${__ITER}-${Date.now()}` }],
    }),
    {
      headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
      timeout: '5s',
    },
  );
  // No pass/fail assertion here: chaos.sh itself checks the properties
  // that matter (at least one success before the kill, at least one
  // success after the restart, nothing hangs) from this run's raw HTTP
  // metrics in its own summary, not from a per-request threshold.
  check(res, { 'request completed (success or clean failure, not a hang)': (r) => r.status !== 0 });
}
