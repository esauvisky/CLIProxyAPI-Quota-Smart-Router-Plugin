package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	uiResourcePath     = "/ui"
	mgmtStateRoutePath = "/plugins/reset-aware-scheduler/state"
)

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func managementRegistrationPayload() managementRegistration {
	return managementRegistration{
		Routes: []managementRoute{
			{
				Method:      http.MethodGet,
				Path:        mgmtStateRoutePath,
				Description: "Returns credentials, quota windows, session bindings and recent decisions of the Reset-Aware Scheduler.",
			},
		},
		Resources: []managementResource{
			{
				Path:        uiResourcePath,
				Menu:        "Reset-Aware Scheduler",
				Description: "Live view of the quota windows, sessions and routing decisions the scheduler uses.",
			},
		},
	}
}

func handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	path := strings.TrimRight(req.Path, "/")
	switch {
	case strings.HasSuffix(path, mgmtStateRoutePath):
		if req.Method != http.MethodGet {
			return nil, fmt.Errorf("management route %s: unsupported method %q", path, req.Method)
		}
		body, errMarshal := json.Marshal(buildUIState())
		if errMarshal != nil {
			return nil, fmt.Errorf("marshal ui state: %w", errMarshal)
		}
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"application/json; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: body,
		})
	case strings.HasSuffix(path, uiResourcePath):
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: []byte(uiHTML),
		})
	default:
		return nil, fmt.Errorf("unknown management path %q", req.Path)
	}
}

const uiHTML = `<!doctype html>
<html lang="en" data-theme="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Reset-Aware Scheduler</title>
<style>
  :root, [data-theme='dark'] {
    --bg-secondary: #151412;
    --bg-primary: #1d1b18;
    --bg-tertiary: #262320;
    --bg-hover: #2e2a26;
    --text-primary: #f6f4f1;
    --text-secondary: #c9c3bb;
    --text-tertiary: #9c958d;
    --border-color: #3a3530;
    --border-hover: #5a544d;
    --primary-color: #8b8680;
    --success-color: #10b981;
    --quota-medium-color: #ffd862;
    --error-color: #c65746;
    --viz-success: #0fae78;
    --viz-failure: #cf4b3a;
    --success-badge-bg: rgba(6, 78, 59, 0.3);
    --success-badge-text: #6ee7b7;
    --success-badge-border: #059669;
    --failure-badge-bg: rgba(198, 87, 70, 0.24);
    --failure-badge-text: #f1b0a6;
    --failure-badge-border: rgba(198, 87, 70, 0.5);
    --warn-badge-bg: rgba(224, 170, 20, 0.18);
    --warn-badge-text: #ffd862;
    --count-badge-bg: rgba(139, 134, 128, 0.28);
    --winner-bg: rgba(16, 185, 129, 0.14);
    --shadow-sm: 0 1px 3px 0 rgb(0 0 0 / 0.3);
    --radius-md: 8px;
    --radius-lg: 12px;
    --radius-full: 9999px;
  }

  [data-theme='light'] {
    --bg-secondary: #faf9f5;
    --bg-primary: #f0eee8;
    --bg-tertiary: #e9e6df;
    --bg-hover: #e9e6df;
    --text-primary: #2d2a26;
    --text-secondary: #6d6760;
    --text-tertiary: #a29c95;
    --border-color: #e3e1db;
    --border-hover: #cecac4;
    --primary-color: #8b8680;
    --quota-medium-color: #e0aa14;
    --viz-success: #10b981;
    --viz-failure: #c65746;
    --success-badge-bg: #d1fae5;
    --success-badge-text: #065f46;
    --success-badge-border: #6ee7b7;
    --failure-badge-bg: rgba(198, 87, 70, 0.14);
    --failure-badge-text: #8a3a30;
    --failure-badge-border: rgba(198, 87, 70, 0.35);
    --warn-badge-bg: rgba(224, 170, 20, 0.16);
    --warn-badge-text: #7a5a00;
    --count-badge-bg: rgba(139, 134, 128, 0.18);
    --winner-bg: rgba(16, 185, 129, 0.12);
    --shadow-sm: 0 1px 2px 0 rgb(0 0 0 / 0.05);
  }

  [data-theme='white'] {
    --bg-secondary: #ffffff;
    --bg-primary: #ffffff;
    --bg-tertiary: #f6f6f6;
    --bg-hover: #f6f6f6;
    --text-primary: #2d2a26;
    --text-secondary: #6d6760;
    --text-tertiary: #a29c95;
    --border-color: #e5e5e5;
    --border-hover: #cccccc;
    --primary-color: #8b8680;
    --quota-medium-color: #e0aa14;
    --viz-success: #10b981;
    --viz-failure: #c65746;
    --success-badge-bg: #d1fae5;
    --success-badge-text: #065f46;
    --success-badge-border: #6ee7b7;
    --failure-badge-bg: rgba(198, 87, 70, 0.14);
    --failure-badge-text: #8a3a30;
    --failure-badge-border: rgba(198, 87, 70, 0.35);
    --warn-badge-bg: rgba(224, 170, 20, 0.16);
    --warn-badge-text: #7a5a00;
    --count-badge-bg: rgba(139, 134, 128, 0.18);
    --winner-bg: rgba(16, 185, 129, 0.12);
    --shadow-sm: 0 1px 2px 0 rgb(0 0 0 / 0.05);
  }

  * { box-sizing: border-box; }
  html, body {
    margin: 0;
    padding: 0;
    background-color: var(--bg-secondary);
    color: var(--text-primary);
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'Roboto', 'Oxygen', 'Ubuntu', 'Cantarell', sans-serif;
    font-size: 14px;
    line-height: 1.5;
    -webkit-font-smoothing: antialiased;
  }
  ::-webkit-scrollbar { width: 8px; height: 8px; }
  ::-webkit-scrollbar-track { background: var(--bg-secondary); }
  ::-webkit-scrollbar-thumb { background: var(--border-color); border-radius: 9999px; }

  .main-content {
    padding: 70px clamp(20px, 3vw, 48px) 40px;
    display: flex;
    flex-direction: column;
    gap: 20px;
    max-width: 100%;
  }
  .page-header { display: flex; flex-direction: column; gap: 6px; padding-right: 180px; }
  .page-title { margin: 0; font-size: 28px; font-weight: 700; line-height: 1.2; letter-spacing: -0.01em; }
  .page-desc { margin: 0; color: var(--text-secondary); }

  .status-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 10px 14px;
    border-radius: 10px;
    border: 1px solid color-mix(in srgb, var(--border-color) 75%, transparent);
    background: var(--bg-primary);
    flex-wrap: wrap;
  }
  .status-pills { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .status-pill {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 5px 12px;
    border-radius: var(--radius-full);
    border: 1px solid color-mix(in srgb, var(--border-color) 75%, transparent);
    background: var(--bg-secondary);
    font-size: 12px;
    font-weight: 600;
    white-space: nowrap;
  }
  .status-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--success-color); box-shadow: 0 0 6px rgba(16, 185, 129, 0.5); }
  .status-dot.err { background: var(--error-color); box-shadow: 0 0 6px rgba(198, 87, 70, 0.6); }
  .status-label { color: var(--text-secondary); }
  .status-value { color: var(--text-primary); font-weight: 700; }

  .toolbar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .btn {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--border-color);
    border-radius: var(--radius-md);
    padding: 7px 14px;
    font-size: 13px;
    font-weight: 600;
    cursor: pointer;
    background-color: var(--bg-tertiary);
    color: var(--text-primary);
  }
  .btn:hover { background-color: var(--bg-hover); border-color: var(--border-hover); }
  .btn:disabled { opacity: 0.6; cursor: progress; }
  .input {
    border: 1px solid var(--border-color);
    border-radius: var(--radius-md);
    padding: 7px 11px;
    font-size: 13px;
    background-color: var(--bg-secondary);
    color: var(--text-primary);
    width: 220px;
  }

  .banner {
    padding: 10px 14px;
    border-radius: var(--radius-md);
    font-size: 13px;
    font-weight: 500;
    background: var(--failure-badge-bg);
    color: var(--failure-badge-text);
    border: 1px solid var(--failure-badge-border);
    white-space: pre-wrap;
    word-break: break-word;
  }
  .banner strong { display: block; margin-bottom: 4px; }

  .card {
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: var(--radius-lg);
    padding: 16px;
    box-shadow: var(--shadow-sm);
  }
  .card-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 12px;
    padding-bottom: 10px;
    border-bottom: 1px solid color-mix(in srgb, var(--border-color) 70%, transparent);
  }
  .card-title { margin: 0; font-size: 15px; font-weight: 700; }

  .cfg-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 10px 20px; }
  .cfg-item { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
  .cfg-key { color: var(--text-tertiary); font-size: 11px; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; }
  .cfg-val { word-break: break-all; }

  .table-wrap { overflow-x: auto; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th, td {
    padding: 8px 10px;
    border-bottom: 1px solid color-mix(in srgb, var(--border-color) 65%, transparent);
    text-align: left;
    vertical-align: top;
  }
  th { color: var(--text-tertiary); font-size: 11px; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; white-space: nowrap; }
  tbody tr:hover { background-color: color-mix(in srgb, var(--bg-tertiary) 65%, transparent); }
  tbody tr:last-child td { border-bottom: 0; }
  td.nowrap { white-space: nowrap; }
  .empty { text-align: center; padding: 24px; color: var(--text-tertiary); }

  .badge {
    display: inline-flex;
    align-items: center;
    padding: 1px 8px;
    font-size: 11px;
    font-weight: 600;
    border-radius: var(--radius-full);
    background: var(--count-badge-bg);
    color: var(--text-secondary);
    white-space: nowrap;
  }
  .badge.ok { background: var(--success-badge-bg); color: var(--success-badge-text); }
  .badge.bad { background: var(--failure-badge-bg); color: var(--failure-badge-text); }
  .badge.warn { background: var(--warn-badge-bg); color: var(--warn-badge-text); }
  .mono { font-family: ui-monospace, 'SF Mono', 'Cascadia Mono', 'JetBrains Mono', Menlo, Consolas, monospace; font-size: 12px; font-variant-numeric: tabular-nums; }
  .subtle { color: var(--text-tertiary); }
  .small { font-size: 12px; }
  .cred-name { font-weight: 650; }
  .err-box {
    margin-top: 6px;
    padding: 6px 10px;
    border-radius: var(--radius-md);
    background: var(--failure-badge-bg);
    color: var(--failure-badge-text);
    border: 1px solid var(--failure-badge-border);
    font-size: 12px;
    font-weight: 600;
    white-space: pre-wrap;
    word-break: break-word;
  }

  .windows { display: flex; flex-direction: column; gap: 8px; min-width: 260px; }
  .win-head { display: flex; justify-content: space-between; gap: 8px; font-size: 12px; }
  .win-name { font-weight: 650; }
  .win-meta { font-size: 11px; color: var(--text-tertiary); margin-top: 2px; }
  .quota-bar { background: var(--bg-tertiary); height: 7px; border-radius: var(--radius-full); overflow: hidden; margin-top: 3px; }
  .quota-fill { height: 100%; border-radius: var(--radius-full); }
  .quota-fill.high { background: var(--viz-success); }
  .quota-fill.medium { background: var(--quota-medium-color); }
  .quota-fill.low { background: var(--viz-failure); }

  details > summary { cursor: pointer; color: var(--text-secondary); font-size: 12px; font-weight: 600; user-select: none; }
  details > summary:hover { color: var(--text-primary); }
  .kv { margin-top: 6px; display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: 2px 12px; }
  .kv .k { color: var(--text-tertiary); }
  .kv .v { word-break: break-all; }
  .rank { margin-top: 8px; }
  .rank th, .rank td { padding: 4px 8px; white-space: nowrap; }
  .rank tr.winner td { background: var(--winner-bg); font-weight: 650; }
</style>
</head>
<body>
<main class="main-content">
  <header class="page-header">
    <h1 class="page-title">Reset-Aware Scheduler</h1>
    <p class="page-desc">Quota windows, session bindings and routing decisions as the scheduler sees them.</p>
  </header>

  <div class="status-bar">
    <div class="status-pills">
      <span class="status-pill"><span class="status-dot" id="statusDot"></span><span class="status-label">Version</span><span class="status-value mono" id="versionVal">-</span></span>
      <span class="status-pill"><span class="status-label">Credentials</span><span class="status-value" id="credCount">0</span></span>
      <span class="status-pill"><span class="status-label">Sessions</span><span class="status-value" id="sessCount">0</span></span>
      <span class="status-pill"><span class="status-label">Last updated</span><span class="status-value" id="lastUpdated">never</span></span>
    </div>
    <div class="toolbar">
      <input type="password" id="mgmtKey" class="input" placeholder="Management Key" style="display:none;" onkeydown="if (event.key === 'Enter') refresh()">
      <button class="btn" id="refreshBtn" onclick="refresh()">Refresh</button>
    </div>
  </div>

  <div id="errorBanner" class="banner" style="display:none;"></div>
  <div id="hostErrorBanner" class="banner" style="display:none;"></div>

  <section class="card">
    <div class="card-header"><h2 class="card-title">Configuration</h2></div>
    <div class="cfg-grid" id="cfgGrid"></div>
  </section>

  <section class="card">
    <div class="card-header"><h2 class="card-title">Credentials</h2></div>
    <div class="table-wrap">
      <table>
        <thead><tr><th>Credential</th><th>Status</th><th>Pool</th><th>Quota windows</th><th>Source</th></tr></thead>
        <tbody id="credBody"></tbody>
      </table>
    </div>
  </section>

  <section class="card">
    <div class="card-header"><h2 class="card-title">Sessions</h2></div>
    <div class="table-wrap">
      <table>
        <thead><tr><th>Session</th><th>Credential</th><th>Provider</th><th>Model</th><th>Bound</th><th>Last seen</th><th>Idle expires</th></tr></thead>
        <tbody id="sessBody"></tbody>
      </table>
    </div>
  </section>

  <section class="card">
    <div class="card-header"><h2 class="card-title">Recent decisions</h2></div>
    <div class="table-wrap">
      <table>
        <thead><tr><th>Time</th><th>Kind</th><th>Model</th><th>Session</th><th>Chosen</th><th>Reason</th></tr></thead>
        <tbody id="decBody"></tbody>
      </table>
    </div>
  </section>
</main>

<script>
const STATE_API_URL = '/v0/management/plugins/reset-aware-scheduler/state';
const REFRESH_MS = 10000;
const ZERO_TIME = '0001-01-01T00:00:00Z';

let state = null;
let lastOkAt = null;
// Offset between server clock (state.now) and browser clock, so countdowns use server time.
let clockSkewMs = 0;
// Keys of <details> blocks the user opened; kept across re-renders.
const openDetails = new Set();

function syncThemeWithCPA() {
  const root = document.documentElement;
  const apply = (mode) => root.setAttribute('data-theme', mode === 'white' || mode === 'light' ? mode : 'dark');
  // Embedded in the CPA iframe (same origin): follow the parent's theme attribute.
  if (window.parent !== window) {
    const parentDoc = window.parent.document.documentElement;
    const syncFromParent = () => {
      const attr = parentDoc.getAttribute('data-theme');
      apply(attr === 'dark' || attr === 'white' ? attr : 'light');
    };
    syncFromParent();
    new MutationObserver(syncFromParent).observe(parentDoc, { attributes: true, attributeFilter: ['data-theme'] });
    return;
  }
  // Opened standalone: use the CPA theme stored in localStorage, if the user ever set one.
  const raw = localStorage.getItem('cli-proxy-theme');
  if (raw !== null) {
    const s = JSON.parse(raw).state;
    if (s.theme === 'dark' || (s.theme === 'auto' && s.resolvedTheme === 'dark')) { apply('dark'); return; }
    if (s.theme === 'white' || s.theme === 'auto') { apply('white'); return; }
    if (s.theme === 'light') { apply('light'); return; }
  }
  apply(window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
}

function deobfuscateCPASecret(payload) {
  const prefix = 'enc::v1::';
  if (!payload.startsWith(prefix)) return payload;
  const bin = atob(payload.slice(prefix.length));
  const keyBytes = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + window.location.host + '|' + navigator.userAgent);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i) ^ keyBytes[i % keyBytes.length];
  return new TextDecoder().decode(out);
}

function detectStoredManagementKey() {
  // Current CPA UI: zustand-persisted auth store.
  const authRaw = localStorage.getItem('cli-proxy-auth');
  if (authRaw !== null) {
    const k = JSON.parse(authRaw).state.managementKey;
    if (typeof k === 'string' && k.trim()) return k.trim();
  }
  // Legacy CPA UI: obfuscated JSON string under managementKey.
  const legacy = localStorage.getItem('managementKey');
  if (legacy !== null) {
    const dec = deobfuscateCPASecret(legacy);
    // Older builds stored the raw key, newer ones a JSON-encoded string.
    const k = dec.startsWith('"') ? JSON.parse(dec) : dec;
    if (k.trim()) return k.trim();
  }
  return '';
}

function authHeaders() {
  const typed = document.getElementById('mgmtKey').value.trim();
  const key = typed !== '' ? typed : detectStoredManagementKey();
  const headers = {};
  if (key !== '') {
    headers['Authorization'] = 'Bearer ' + key;
    headers['X-Management-Key'] = key;
  }
  return headers;
}

function esc(v) {
  return String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function isZero(t) {
  return !t || t === ZERO_TIME || t.startsWith('0001-01-01');
}

function serverNow() {
  return Date.now() + clockSkewMs;
}

function fmtDuration(ms) {
  const neg = ms < 0;
  let s = Math.floor(Math.abs(ms) / 1000);
  const d = Math.floor(s / 86400); s -= d * 86400;
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  let out;
  if (d > 0) out = d + 'd ' + h + 'h';
  else if (h > 0) out = h + 'h ' + m + 'm';
  else if (m > 0) out = m + 'm ' + s + 's';
  else out = s + 's';
  return neg ? '-' + out : out;
}

function relText(t) {
  const diff = new Date(t).getTime() - serverNow();
  if (Math.abs(diff) < 1000) return 'now';
  return diff > 0 ? 'in ' + fmtDuration(diff) : fmtDuration(-diff) + ' ago';
}

function absText(t) {
  return new Date(t).toLocaleString();
}

// Live-updating relative time; refreshed every second by tick().
function rel(t) {
  if (isZero(t)) return '<span class="subtle">-</span>';
  return '<span class="live-rel" data-t="' + esc(t) + '" title="' + esc(absText(t)) + '">' + esc(relText(t)) + '</span>';
}

function relAbs(t) {
  if (isZero(t)) return '<span class="subtle">unknown</span>';
  return rel(t) + '<div class="win-meta">' + esc(absText(t)) + '</div>';
}

function countdown(t) {
  if (isZero(t)) return '<span class="subtle">-</span>';
  return '<span class="live-cd mono" data-t="' + esc(t) + '" title="' + esc(absText(t)) + '">' + countdownText(t) + '</span>';
}

function countdownText(t) {
  const diff = new Date(t).getTime() - serverNow();
  if (diff <= 0) return '<span class="badge warn">idle, re-picks on next prompt</span>';
  return esc(fmtDuration(diff));
}

function tick() {
  for (const el of document.querySelectorAll('.live-rel')) el.textContent = relText(el.dataset.t);
  for (const el of document.querySelectorAll('.live-cd')) el.innerHTML = countdownText(el.dataset.t);
  if (lastOkAt !== null) {
    document.getElementById('lastUpdated').textContent = fmtDuration(Date.now() - lastOkAt) + ' ago';
  }
}

function pct(fraction) {
  return (fraction * 100).toFixed(1) + '%';
}

function tierOf(fraction) {
  if (fraction >= 0.5) return 'high';
  if (fraction >= 0.2) return 'medium';
  return 'low';
}

function truncate(s, n) {
  return s.length > n ? s.slice(0, n) + '…' : s;
}

function details(key, summary, body) {
  const open = openDetails.has(key) ? ' open' : '';
  return '<details data-key="' + esc(key) + '"' + open + '><summary>' + summary + '</summary>' + body + '</details>';
}

function credName(a) {
  // Label, email and name are each optional on host credentials; show the most specific one present.
  for (const v of [a.label, a.email, a.name]) if (v) return v;
  return a.id;
}

function credNameByID(id) {
  for (const c of state.credentials) if (c.auth.id === id) return credName(c.auth);
  return id;
}

function renderConfig() {
  const c = state.config;
  const items = [
    ['min_remaining', pct(c.min_remaining)],
    ['tie_window_minutes', c.tie_window_minutes + ' min'],
    ['spread.prefix', c.spread.prefix],
    ['spread.idle_minutes', c.spread.idle_minutes + ' min'],
    ['spread.models', c.spread.models === null || c.spread.models.length === 0 ? '(none)' : c.spread.models.join(', ')],
    ['spread.credentials', String(c.spread.credentials === null ? 0 : c.spread.credentials.length)],
    ['management.url', c.management.url === '' ? '(unset)' : c.management.url],
    ['management.refresh_minutes', c.management.refresh_minutes + ' min'],
  ];
  document.getElementById('cfgGrid').innerHTML = items.map(([k, v]) =>
    '<div class="cfg-item"><span class="cfg-key">' + esc(k) + '</span><span class="cfg-val mono">' + esc(v) + '</span></div>'
  ).join('');
}

function renderWindow(w) {
  const r = Math.max(0, Math.min(1, w.remaining));
  const periodH = w.period / 3.6e12;
  const models = w.models && w.models.length > 0 ? w.models.join(', ') : 'all models';
  return '<div>' +
    '<div class="win-head"><span class="win-name">' + esc(w.name) +
      (periodH > 0 ? ' <span class="subtle">(' + esc(periodH >= 24 ? (periodH / 24).toFixed(periodH % 24 ? 1 : 0) + 'd' : periodH.toFixed(periodH % 1 ? 1 : 0) + 'h') + ')</span>' : '') +
    '</span><span class="mono">' + esc(pct(w.remaining)) + '</span></div>' +
    '<div class="quota-bar"><div class="quota-fill ' + tierOf(r) + '" style="width:' + (r * 100).toFixed(1) + '%"></div></div>' +
    '<div class="win-meta">resets ' + (isZero(w.reset_at) ? 'unknown' : rel(w.reset_at) + ' · ' + esc(absText(w.reset_at))) + '</div>' +
    '<div class="win-meta">' + esc(models) + '</div>' +
  '</div>';
}

function renderSignals(key, signals) {
  const keys = Object.keys(signals).sort();
  if (keys.length === 0) return '';
  const rows = keys.map(k => {
    const v = signals[k];
    const text = typeof v === 'object' ? JSON.stringify(v) : String(v);
    return '<span class="k mono">' + esc(k) + '</span><span class="v mono">' + esc(text) + '</span>';
  }).join('');
  return '<div style="margin-top:6px">' + details(key, 'Header signals (' + keys.length + ')', '<div class="kv small">' + rows + '</div>') + '</div>';
}

function renderCredentials() {
  const body = document.getElementById('credBody');
  if (state.credentials.length === 0) {
    body.innerHTML = '<tr><td colspan="5" class="empty">No credentials reported by the host.</td></tr>';
    return;
  }
  body.innerHTML = state.credentials.map(c => {
    const a = c.auth;
    const q = c.quota;
    const name = credName(a);
    const secondary = [a.email, a.name, a.id].filter(v => v && v !== name);

    const status = [];
    if (a.disabled) status.push('<span class="badge bad">disabled</span>');
    if (a.unavailable) status.push('<span class="badge bad">unavailable</span>');
    if (!a.disabled && !a.unavailable) status.push('<span class="badge ok">' + esc(a.status === '' ? 'active' : a.status) + '</span>');
    else if (a.status !== '') status.push('<span class="badge">' + esc(a.status) + '</span>');
    if (!isZero(a.next_retry_after)) status.push('<div class="win-meta">retry ' + rel(a.next_retry_after) + '</div>');

    const pool = c.in_pool
      ? '<span class="badge ok">in pool</span><div class="win-meta">weight <span class="mono">' + esc(c.weight) + '</span></div>'
      : '<span class="badge">not in pool</span>';

    let windows;
    if (!c.has_quota) windows = '<span class="subtle">no snapshot yet</span>';
    else if (q.windows === null || q.windows.length === 0) windows = '<span class="subtle">no windows reported</span>';
    else windows = '<div class="windows">' + q.windows.map(renderWindow).join('') + '</div>';

    let source = '<span class="subtle">-</span>';
    if (c.has_quota) {
      source = '<span class="badge">' + esc(q.source) + '</span>' +
        (q.plan ? ' <span class="badge">' + esc(q.plan) + '</span>' : '') +
        '<div class="win-meta">fetched ' + rel(q.fetched_at) + '</div>';
    }

    return '<tr>' +
      '<td><div class="cred-name">' + esc(name) + '</div>' +
        '<div class="win-meta"><span class="badge">' + esc(a.provider) + '</span> ' + secondary.map(esc).join(' · ') + '</div>' +
        (q.error ? '<div class="err-box">Quota error: ' + esc(q.error) + '</div>' : '') +
        (c.signals ? renderSignals('sig:' + a.id, c.signals) : '') +
      '</td>' +
      '<td class="nowrap">' + status.join(' ') + '</td>' +
      '<td class="nowrap">' + pool + '</td>' +
      '<td>' + windows + '</td>' +
      '<td class="nowrap">' + source + '</td>' +
    '</tr>';
  }).join('');
}

function renderSessions() {
  const body = document.getElementById('sessBody');
  if (state.sessions.length === 0) {
    body.innerHTML = '<tr><td colspan="7" class="empty">No bound sessions.</td></tr>';
    return;
  }
  body.innerHTML = state.sessions.map(s =>
    '<tr>' +
      '<td class="mono nowrap" title="' + esc(s.session) + '">' + esc(truncate(s.session, 24)) + '</td>' +
      '<td>' + esc(credNameByID(s.auth_id)) + '</td>' +
      '<td><span class="badge">' + esc(s.provider) + '</span></td>' +
      '<td class="mono">' + esc(s.model) + '</td>' +
      '<td class="nowrap">' + rel(s.bound_at) + '</td>' +
      '<td class="nowrap">' + rel(s.last_seen) + '</td>' +
      '<td class="nowrap">' + countdown(s.idle_expires_at) + '</td>' +
    '</tr>'
  ).join('');
}

function num(v, digits) {
  return Number.isFinite(v) ? v.toFixed(digits) : String(v);
}

function renderRanking(d) {
  const rows = d.ranking.map(r =>
    '<tr class="' + (r.auth_id === d.auth_id ? 'winner' : '') + '">' +
      '<td>' + esc(credNameByID(r.auth_id)) + '</td>' +
      '<td>' + esc(r.provider) + '</td>' +
      '<td class="mono">' + esc(num(r.weight, 2)) + '</td>' +
      '<td>' + esc(r.window ? r.window : '-') + '</td>' +
      '<td class="mono">' + esc(pct(r.remaining)) + '</td>' +
      '<td class="mono">' + esc(num(r.hours_to_reset, 2)) + '</td>' +
      '<td class="mono">' + esc(num(r.rate, 4)) + '</td>' +
      '<td>' + (r.excluded ? '<span class="badge bad">' + esc(r.excluded) + '</span>' : '') + '</td>' +
    '</tr>'
  ).join('');
  return '<table class="rank"><thead><tr><th>Auth</th><th>Provider</th><th>Weight</th><th>Window</th><th>Remaining</th><th>Hours to reset</th><th>Rate</th><th>Excluded</th></tr></thead><tbody>' + rows + '</tbody></table>';
}

function renderDecisions() {
  const body = document.getElementById('decBody');
  if (state.decisions.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="empty">No decisions recorded yet.</td></tr>';
    return;
  }
  const sorted = state.decisions.slice().sort((a, b) => new Date(b.time) - new Date(a.time));
  body.innerHTML = sorted.map(d => {
    const key = 'dec:' + d.time + ':' + d.kind + ':' + d.session + ':' + d.model;
    const ranking = d.ranking && d.ranking.length > 0
      ? details(key, 'Ranking (' + d.ranking.length + ')', renderRanking(d))
      : '';
    return '<tr>' +
      '<td class="nowrap">' + rel(d.time) + '<div class="win-meta">' + esc(absText(d.time)) + '</div></td>' +
      '<td><span class="badge">' + esc(d.kind) + '</span></td>' +
      '<td class="mono">' + esc(d.model) + '</td>' +
      '<td class="mono nowrap" title="' + esc(d.session ? d.session : '') + '">' + (d.session ? esc(truncate(d.session, 16)) : '<span class="subtle">-</span>') + '</td>' +
      '<td>' + (d.auth_id ? esc(credNameByID(d.auth_id)) : '<span class="badge bad">none</span>') + '</td>' +
      '<td>' + esc(d.reason) + ranking + '</td>' +
    '</tr>';
  }).join('');
}

function render() {
  const hostBanner = document.getElementById('hostErrorBanner');
  if (state.error) {
    hostBanner.innerHTML = '<strong>Scheduler could not read the host credential list</strong>' + esc(state.error);
    hostBanner.style.display = 'block';
  } else {
    hostBanner.style.display = 'none';
  }
  document.getElementById('versionVal').textContent = state.version;
  document.getElementById('credCount').textContent = String(state.credentials.length);
  document.getElementById('sessCount').textContent = String(state.sessions.length);
  renderConfig();
  renderCredentials();
  renderSessions();
  renderDecisions();
}

function showError(title, detail) {
  const banner = document.getElementById('errorBanner');
  const stale = lastOkAt === null
    ? 'No data loaded yet.'
    : 'Showing STALE data from ' + new Date(lastOkAt).toLocaleTimeString() + '.';
  banner.innerHTML = '<strong>' + esc(title) + '</strong>' + esc(detail) + '\n' + esc(stale);
  banner.style.display = 'block';
  document.getElementById('statusDot').classList.add('err');
}

function clearError() {
  document.getElementById('errorBanner').style.display = 'none';
  document.getElementById('statusDot').classList.remove('err');
}

let inFlight = false;
async function refresh() {
  if (inFlight) return;
  inFlight = true;
  const btn = document.getElementById('refreshBtn');
  btn.disabled = true;
  try {
    let resp;
    try {
      resp = await fetch(STATE_API_URL, { method: 'GET', headers: authHeaders(), cache: 'no-store' });
    } catch (err) {
      showError('Network error fetching ' + STATE_API_URL, String(err));
      return;
    }
    const text = await resp.text();
    if (resp.status === 401 || resp.status === 403) {
      document.getElementById('mgmtKey').style.display = 'inline-block';
      showError('HTTP ' + resp.status + ': management key required or rejected. Enter it above and press Refresh.', text);
      return;
    }
    if (!resp.ok) {
      showError('HTTP ' + resp.status + ' ' + resp.statusText + ' from ' + STATE_API_URL, text);
      return;
    }
    let next;
    try {
      next = JSON.parse(text);
    } catch (err) {
      showError('Invalid JSON from ' + STATE_API_URL + ': ' + err.message, truncate(text, 2000));
      return;
    }
    // Go encodes empty slices as null; normalize so renderers can rely on arrays.
    for (const k of ['credentials', 'sessions', 'decisions']) if (next[k] === null) next[k] = [];
    state = next;
    clockSkewMs = new Date(state.now).getTime() - Date.now();
    render();
    lastOkAt = Date.now();
    clearError();
    tick();
  } finally {
    inFlight = false;
    btn.disabled = false;
  }
}

document.addEventListener('toggle', (ev) => {
  const el = ev.target;
  if (el.tagName !== 'DETAILS') return;
  if (el.open) openDetails.add(el.dataset.key);
  else openDetails.delete(el.dataset.key);
}, true);

window.addEventListener('error', (ev) => showError('Page script error', String(ev.message)));
window.addEventListener('unhandledrejection', (ev) => showError('Page script error', String(ev.reason)));

syncThemeWithCPA();
if (detectStoredManagementKey() === '') document.getElementById('mgmtKey').style.display = 'inline-block';
void refresh();
setInterval(() => void refresh(), REFRESH_MS);
setInterval(tick, 1000);
</script>
</body>
</html>
`
