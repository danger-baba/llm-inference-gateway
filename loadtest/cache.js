// Drives 70% repeat traffic (drawn from a small fixed pool of questions)
// against the mock provider, and reports exact, semantic, and combined
// hit rates plus tokens avoided, read from the gateway's own /metrics
// (README, Load testing).
//
// Requires deploy/loadtest.config.yaml's cache.semantic to be enabled
// (it is, by default) with real ONNX Runtime/model assets reachable --
// see docs/adr/0012 and make download-embedding-model.
//
// setup()/teardown() snapshot the gateway's own counters before and
// after this run and report the *difference* -- gateway_cache_hits_total
// etc. are cumulative from process start, not scoped to one k6 run, so
// diffing is what makes this correct even against a gateway that's
// already served other traffic (a fresh restart isn't required, though
// it does make the numbers easier to eyeball directly on /metrics).
//
// Usage:
//   make mock-provider                                  # separate terminal
//   k6 run loadtest/cache.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { fetchMetrics, parseCounterValue } from './lib/metrics.js';

const BASE_URL = __ENV.GATEWAY_URL || 'http://127.0.0.1:18080';
const TOKEN = __ENV.LOADTEST_TOKEN || 'sk-vk-loadtest';
const CHAT_URL = `${BASE_URL}/v1/chat/completions`;
const METRICS_URL = `${BASE_URL}/metrics`;

// A fixed pool the 70% "repeat" share is drawn from. Kept deliberately
// small and literal-question-like (not paraphrased) so an exact-cache
// hit is the expected outcome for a repeat, not a semantic near-miss --
// this script is measuring hit rate under realistic repeat traffic, not
// re-running Phase 7's threshold eval.
const REPEAT_POOL = [
  'What is the capital of France?',
  'How do I reset my password?',
  "What's the weather like today?",
  'Summarize this article for me.',
  'Translate this sentence to French.',
  'How much RAM does my laptop have?',
  'Write a poem about the ocean.',
  'Explain quantum entanglement simply.',
  'List three benefits of exercise.',
  'What time is it in Tokyo?',
];

// The "fresh" 30% share needs to be genuinely semantically distinct from
// both the repeat pool and each other, not just textually different --
// an early version of this script appended only numbers to a shared
// sentence prefix ("one-off question ... 3-45-1785584000123"), and the
// semantic cache correctly recognized those as near-duplicates of each
// other, inflating the measured semantic hit rate. Combining a random
// subject with a random template gives real semantic spread; the
// appended VU/iter/timestamp still guarantees the exact string itself
// is unique, so it never collides with the exact-match tier either.
const FRESH_SUBJECTS = [
  'giraffes', 'volcanoes', 'jazz music', 'the stock market', 'medieval castles',
  'coral reefs', 'chess openings', 'renewable energy', 'ancient Rome',
  'quantum computers', 'tropical rainforests', 'the human immune system',
  'glacier formation', 'the printing press', 'octopus intelligence',
  'Formula 1 racing', 'the Great Wall of China', 'sourdough bread',
  'black holes', 'origami', 'the Amazon river', 'bee colonies',
  'Renaissance painting', 'earthquake prediction',
];
const FRESH_TEMPLATES = [
  (s) => `Tell me an interesting fact about ${s}.`,
  (s) => `Why should I care about ${s}?`,
  (s) => `Give a short history of ${s}.`,
  (s) => `What's a common misconception about ${s}?`,
];

export const options = {
  scenarios: {
    cache: {
      executor: 'constant-arrival-rate',
      rate: 20,
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 20,
      maxVUs: 50,
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
  },
};

function snapshot(text) {
  return {
    exact: parseCounterValue(text, 'gateway_cache_hits_total{tier="exact"}'),
    semantic: parseCounterValue(text, 'gateway_cache_hits_total{tier="semantic"}'),
    tokensSaved: parseCounterValue(text, 'gateway_tokens_saved_total'),
  };
}

export function setup() {
  return snapshot(fetchMetrics(http, METRICS_URL));
}

export default function () {
  const isRepeat = Math.random() < 0.7;
  const content = isRepeat
    ? REPEAT_POOL[Math.floor(Math.random() * REPEAT_POOL.length)]
    : (() => {
        const subject = FRESH_SUBJECTS[Math.floor(Math.random() * FRESH_SUBJECTS.length)];
        const template = FRESH_TEMPLATES[Math.floor(Math.random() * FRESH_TEMPLATES.length)];
        return `${template(subject)} (ref ${__VU}-${__ITER}-${Date.now()})`;
      })();

  const body = JSON.stringify({
    model: 'fast',
    temperature: 0, // required for cache eligibility -- see internal/cache/exact.Eligible
    messages: [{ role: 'user', content }],
  });
  const res = http.post(CHAT_URL, body, {
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
  });
  check(res, { 'status is 200': (r) => r.status === 200 });
}

export function teardown(before) {
  sleep(1);
  const after = snapshot(fetchMetrics(http, METRICS_URL));

  const exactHits = after.exact - before.exact;
  const semanticHits = after.semantic - before.semantic;
  const tokensSaved = after.tokensSaved - before.tokensSaved;

  console.log('==== Cache effectiveness (gateway-measured, diffed against a pre-run snapshot) ====');
  console.log(`  exact-cache hits this run:    ${exactHits}`);
  console.log(`  semantic-cache hits this run: ${semanticHits}`);
  console.log(`  tokens avoided this run:      ${tokensSaved}`);
  console.log('  Divide (exact + semantic) by the "iterations" count in the summary');
  console.log('  below for the combined hit rate.');
}
