// Small helpers for reading the gateway's own Prometheus text-format
// output from inside a k6 script, so a load test can report the
// gateway's own measured numbers (e.g. gateway_proxy_overhead_seconds)
// rather than only k6's client-observed timings. Deliberately hand-rolled
// rather than a dependency: k6's JS runtime isn't Node, and the exposition
// format is simple enough that a small parser is less risk than a new
// import to vet.

// fetchMetrics GETs url (the gateway's /metrics endpoint) and returns the
// raw Prometheus text-format body.
export function fetchMetrics(http, url) {
  const res = http.get(url);
  if (res.status !== 200) {
    throw new Error(`GET ${url} = ${res.status}, want 200`);
  }
  return res.body;
}

// parseHistogram extracts one histogram metric's buckets, sum, and count
// from Prometheus text-format output, ignoring any other labels present
// (e.g. gateway_provider_duration_seconds's provider/model labels) --
// callers that need a specific label combination should pass a
// metricName that already includes the label selector, e.g.
// `gateway_provider_duration_seconds{provider="mock1",model="mock-model-v1"}`
// is not supported directly; this is written for the two *unlabelled*
// histograms the README calls out (gateway_request_duration_seconds,
// gateway_proxy_overhead_seconds).
export function parseHistogram(text, metricName) {
  const buckets = [];
  let sum = 0;
  let count = 0;
  for (const line of text.split('\n')) {
    if (line.startsWith('#')) continue;
    if (line.startsWith(metricName + '_bucket')) {
      const m = line.match(/le="([^"]+)"\}\s+(\S+)/);
      if (m) {
        const le = m[1] === '+Inf' ? Infinity : parseFloat(m[1]);
        buckets.push([le, parseFloat(m[2])]);
      }
    } else if (line.startsWith(metricName + '_sum ')) {
      sum = parseFloat(line.trim().split(/\s+/).pop());
    } else if (line.startsWith(metricName + '_count ')) {
      count = parseFloat(line.trim().split(/\s+/).pop());
    }
  }
  buckets.sort((a, b) => a[0] - b[0]);
  return { buckets, sum, count };
}

// quantile approximates the qth quantile (0..1) from a parsed histogram
// via linear interpolation within the bucket the target rank falls in --
// the same approach Prometheus's own histogram_quantile() uses. Returns
// null if the histogram has no observations at all.
export function quantile(hist, q) {
  if (!hist.count) return null;
  const target = hist.count * q;
  let prevLe = 0;
  let prevCount = 0;
  for (const [le, cumCount] of hist.buckets) {
    if (cumCount >= target) {
      if (le === Infinity) return prevLe;
      const span = cumCount - prevCount;
      const frac = span === 0 ? 0 : (target - prevCount) / span;
      return prevLe + frac * (le - prevLe);
    }
    prevLe = le;
    prevCount = cumCount;
  }
  return prevLe;
}

// parseCounterValue reads a single counter/gauge sample's value by
// matching the full metric-and-label text exactly as it appears in the
// exposition format, e.g. parseCounterValue(text, 'gateway_cache_hits_total{tier="exact"}').
export function parseCounterValue(text, metricAndLabels) {
  for (const line of text.split('\n')) {
    if (line.startsWith('#')) continue;
    if (line.startsWith(metricAndLabels)) {
      const parts = line.trim().split(/\s+/);
      return parseFloat(parts[parts.length - 1]);
    }
  }
  return 0;
}
