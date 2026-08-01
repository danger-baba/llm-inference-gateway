// Ramps arrival rate against the mock provider (fixed latency, see
// deploy/loadtest.config.yaml) to find sustainable RPS, and reports
// gateway_proxy_overhead_seconds read directly off the gateway's own
// /metrics -- not derived from k6's client-side timing -- so gateway
// overhead is separable from total request time (README, Load testing).
//
// Every request uses a nonzero temperature and unique content so nothing
// here is ever cache-eligible: this script measures the real proxy path,
// not a cache hit's near-zero overhead.
//
// gateway_proxy_overhead_seconds/gateway_request_duration_seconds are
// cumulative from process start, so run this against a just-started
// gateway (as the usage below does) for the histogram to reflect only
// this run -- if you've already run another script against the same
// process, restart it first.
//
// Usage:
//   make mock-provider                                  # separate terminal
//   k6 run loadtest/overhead.js
//   k6 run -e GATEWAY_URL=http://127.0.0.1:18080 loadtest/overhead.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { fetchMetrics, parseHistogram, quantile } from './lib/metrics.js';

const BASE_URL = __ENV.GATEWAY_URL || 'http://127.0.0.1:18080';
const TOKEN = __ENV.LOADTEST_TOKEN || 'sk-vk-loadtest';
const CHAT_URL = `${BASE_URL}/v1/chat/completions`;
const METRICS_URL = `${BASE_URL}/metrics`;

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-arrival-rate',
      startRate: 5,
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 300,
      stages: [
        { target: 20, duration: '15s' },
        { target: 50, duration: '30s' },
        { target: 100, duration: '30s' },
        { target: 150, duration: '30s' },
        { target: 0, duration: '10s' },
      ],
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  const body = JSON.stringify({
    model: 'fast',
    temperature: 0.7,
    messages: [{ role: 'user', content: `overhead probe ${__VU}-${__ITER}-${Date.now()}` }],
  });
  const res = http.post(CHAT_URL, body, {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
  });
  check(res, { 'status is 200': (r) => r.status === 200 });
}

export function teardown() {
  sleep(1); // give the async ledger/metrics path a moment to settle
  const text = fetchMetrics(http, METRICS_URL);

  const overhead = parseHistogram(text, 'gateway_proxy_overhead_seconds');
  const total = parseHistogram(text, 'gateway_request_duration_seconds');

  console.log('==== gateway_proxy_overhead_seconds (gateway-measured) ====');
  console.log(`  samples: ${overhead.count}`);
  console.log(`  p50: ${quantile(overhead, 0.5)}s`);
  console.log(`  p95: ${quantile(overhead, 0.95)}s`);
  console.log(`  p99: ${quantile(overhead, 0.99)}s`);
  console.log('==== gateway_request_duration_seconds (gateway-measured, end-to-end) ====');
  console.log(`  samples: ${total.count}`);
  console.log(`  p50: ${quantile(total, 0.5)}s`);
  console.log(`  p95: ${quantile(total, 0.95)}s`);
  console.log(`  p99: ${quantile(total, 0.99)}s`);
  console.log('Sustainable RPS: read http_reqs rate from the summary above -- the');
  console.log('highest ramp stage where http_req_failed stayed under the 1% threshold.');
}
