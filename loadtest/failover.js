// Drives steady traffic against a gateway configured with a "primary"
// provider that fails 100% of the time (429) and a healthy "secondary"
// behind it, and asserts the *client-visible* success rate stays high
// despite primary's total failure (README, Load testing).
//
// Uses deploy/config.yaml as-is: it already sets up exactly this
// primary-always-429/secondary-always-succeeds scenario for the same
// purpose (manually curling to watch X-Gateway-Attempts) -- see that
// file's own header comment.
//
// Usage:
//   POSTGRES_DSN="postgres://fake:fake@127.0.0.1:1/fakedb" \
//     go run ./cmd/gateway --config deploy/config.yaml       # separate terminal, addr overridden to a free port
//   k6 run loadtest/failover.js

import http from 'k6/http';
import { check } from 'k6';
import { Rate } from 'k6/metrics';

const BASE_URL = __ENV.GATEWAY_URL || 'http://127.0.0.1:18080';
const TOKEN = __ENV.LOADTEST_TOKEN || 'sk-vk-loadtest';
const CHAT_URL = `${BASE_URL}/v1/chat/completions`;

const clientVisibleSuccess = new Rate('client_visible_success_rate');

export const options = {
  scenarios: {
    failover: {
      // Throttled, not unthrottled constant-VUs: deploy/config.yaml's
      // rate_limit.default_tpm (200000) is sized for a real demo
      // workload, not an unbounded loop across 10 VUs -- an earlier,
      // unthrottled version of this script drove ~1,700 req/s and made
      // the gateway's own token-bucket limiter, not primary's failure,
      // the dominant source of non-200 responses. 15/s keeps this run's
      // token spend comfortably inside that budget so what's being
      // measured is actually failover behavior.
      executor: 'constant-arrival-rate',
      rate: 15,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 20,
      maxVUs: 50,
    },
  },
  thresholds: {
    // The gate: even with primary at 100% failure, a client behind the
    // gateway should see success almost every time, via secondary.
    client_visible_success_rate: ['rate>0.99'],
  },
};

export default function () {
  const body = JSON.stringify({
    model: 'fast',
    temperature: 0.7, // cache-ineligible -- every request must really reach a provider
    messages: [{ role: 'user', content: `failover probe ${__VU}-${__ITER}-${Date.now()}` }],
  });
  const res = http.post(CHAT_URL, body, {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
  });
  const ok = res.status === 200;
  clientVisibleSuccess.add(ok);
  check(res, {
    'status is 200 despite primary always failing': () => ok,
    'served by secondary': (r) => ok && r.headers['X-Gateway-Provider'] === 'secondary',
  });
}
