# CPA Free-Tier Plugin

<p align="center">
  <img src="https://img.shields.io/badge/CLIProxyAPI-ABI%20v1%20Compliant-00ADD8?style=for-the-badge&logo=go" alt="CPA ABI v1" />
  <img src="https://img.shields.io/badge/Platforms-Linux%20%7C%20Darwin%20%7C%20Windows-4A154B?style=for-the-badge" alt="Cross Platform" />
  <img src="https://img.shields.io/badge/Release-v1.5.3-success?style=for-the-badge" alt="Release" />
  <img src="https://img.shields.io/badge/License-MIT-blue?style=for-the-badge" alt="License MIT" />
</p>

A high-performance, native dynamic C ABI v1 plugin (`.so` / `.dylib` / `.dll`) for **[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** (v7+). It provides out-of-the-box access to **24 curated, 100% free AI models** across **OpenCode Zen** (10 models) and **Kilo AI** (14 models), plus the models your **OmniRush** account is actually served, alongside a native **Management Center Web Dashboard** directly accessible in the CPA UI without requiring user API keys or subscriptions.

---

## 🌟 Key Features (v1.5.3)

* **Zero-Key & Auto-Registered:** Ready to use immediately. Automatically writes its provider auth descriptor (`~/.cli-proxy-api/cpa-freetier-plugin.json`) on `cliproxy_plugin_init`, `plugin.register`, and `plugin.reconfigure` unless disabled via `CPA_NO_AUTH_DESCRIPTOR=1`.
* **Native Management Center Dashboard (`management_api`):**
  * Registers directly into the CLIProxyAPI Management Center UI sidebar under **"Free Tier Plugin"** (`/v0/resource/plugins/cpa-freetier-plugin/dashboard`).
  * **1-Click OmniRush Device Flow:** Complete OAuth 2.0 Device Code Authorization directly in the browser; polls and stores rotated tokens to `~/.omnirush/auth.json` atomically with `0600` permissions.
  * **Interactive Model Fleet Manager:** Dynamically toggle active/excluded models on the fly; updates `excluded_models` in `cpa-freetier-plugin.json` without restarting CPA.
  * **Network & Proxy Diagnostic Matrix:** Visual status indicators for the embedded native harness (preamble size, tool count, cached sessions), the configured proxy pool (direct when none is set), and upstream latency pinging.
* **OmniRush Frontier Integration:**
  * Speaks the gateway's **OpenAI Responses API** (`POST https://omnirush.ai/omnirush/v1/responses`) and translates both directions: Chat Completions requests in, `response.output_text.delta` / `function_call` / `response.completed` SSE events back out as standard `chat.completion.chunk` frames (text, reasoning, tool calls and usage all preserved).
  * **Live model catalog:** the advertised OmniRush ids come from the account's own `GET /v1/models` (cached 10 minutes), so a model the gateway starts or stops serving needs no plugin release. Only ids the account can actually call are listed.
  * Automatic token refresh and rotation with mutex-locked grace window handling on HTTP `401`.
* **OpenCode v2 Wire Protocol & Automatic Sidecar Routing:**
  * Implements the OpenCode v2 CLI contract (`opencode/latest/2.0.32/cli`), descending-timestamp session affinity (`ses_<12-hex><14-base62>`, 30-minute TTL with automatic expiry sweeping), and automatic `read`/`shell` fingerprint tool injection (renaming caller `read`/`shell` tools to `read_client`/`shell_client` on send and restoring them transparently in responses).
  * **Local Sidecar Auto-Detection (`127.0.0.1:8789`):** Automatically probes `127.0.0.1:8789` (50ms TCP timeout) or uses `OPENCODE_BASE_URL` to route OpenCode requests through a local Bun/TLS sidecar (such as [`zen-proxy`](https://github.com/12errh/zen-proxy)) when available, bypassing upstream TLS/JA3 fingerprinting and 12-tool manifest checks (`429 FreeUsageLimitError` / `403 FreeTierError`) without double-proxying.
* **Universal Agent & Client Compatibility (`request.normalize` + Executor Sanitizer):**
  * **Parameter Stripping:** Strips unsupported parameters (`thinking`, `enable_thinking`, `thinking_budget`, `thinking_config`, `search`, `web_search`, `return_citations`) sent by coding agents (**Hermes Agent**, **Cursor**, **Cline**, **Claude Code**, **Aider**, **Synaplan**). `reasoning_effort` is deliberately **preserved** because the upstream gateway accepts it and adjusts reasoning depth. Custom overrides via `CPA_STRIP_FIELDS` and `CPA_KEEP_FIELDS`.
  * **Message Role & Schema Normalization:** Converts legacy `prompt` strings into standard `messages`, populates empty `messages: []` arrays, normalizes empty/missing/unrecognized roles (`role: ""`) to `"user"`, maps `"developer"` to `"system"`, lowercases standard roles (`system`, `user`, `assistant`, `tool`, `function`), and repairs `nil` message content to `""`.
* **Operator Model Exclusions:** Hide specific models from `/v1/models` and block execution (`404 model_not_found`) dynamically via `CPA_EXCLUDED_MODELS` or `"excluded_models"` / `"excluded-models"` in the auth descriptor JSON (supports case-insensitive globs like `muse*` and prefixes `opencode/` or `kilo/`, cached with a 2-second TTL).
* **Automatic Model Health Quarantine:** Models that return upstream `400` / `404` / `410` with missing or deprecated markers (`not found`, `does not exist`, `unknown model`, `unknown provider`, `model is unavailable`, `model_not_found`, `modeldeprecated`, `deprecated`) are automatically hidden from `listModels()` for 30 minutes (`modelUnhealthyTTL`) and restored on recovery.
* **Dual Execution Modes:**
  * **Native Zero-Buffering Streaming (`executor.execute_stream`):** Emits SSE chunks directly via `host.stream.emit` (up to 16 MiB per `data:` line) with guaranteed single-close semantics (`host.stream.close`).
  * **Intelligent Non-Streaming Assembler (`executor.execute`):** Assembles upstream SSE streams into standard OpenAI `chat.completion` JSON objects, preserving `content`, `reasoning_content`, indexed `tool_calls`, `finish_reason`, and `usage`.
* **Full Multi-Platform Coverage:** Pre-compiled C-shared libraries for **Linux** (`amd64`, `arm64`), **macOS** (`darwin_arm64`, `darwin_amd64`), and **Windows** (`windows_amd64`).

---

## 📋 Supported Free Models (24 curated + live OmniRush catalog)

Bare model IDs (e.g. `flint-40b-free`, `kilo-auto/free`, `gpt-6-astra`) as well as provider-prefixed IDs (`opencode/<id>`, `kilo/<id>`, `omnirush/<id>`) are accepted when calling the plugin.

### 1. OmniRush Frontier Models (Live Gateway Catalog)

The plugin asks the account's gateway (`GET <gateway>/models`) which ids it is served, caches the answer for 10 minutes, and advertises exactly those. When the gateway is unreachable the known-good fallback set is used.

| Model Identifier | Display Name in Code | Context Window | Max Output | Thinking / Reasoning Levels |
| :--- | :--- | :--- | :--- | :---: |
| `gpt-6-astra` | `GPT 6 Astra (OmniRush)` | **400,000** | 128,000 | ✅ (`low`, `high`, `xhigh`, `max`) |
| `gpt-6.1-sol` | `GPT 6.1 Sol (OmniRush)` | **400,000** | 128,000 | ✅ (`low`, `high`, `xhigh`, `max`) |
| `gpt-6-sol` | `GPT 6 Sol (OmniRush)` | **400,000** | 128,000 | ✅ (`low`, `high`, `xhigh`, `max`) |

> **Why the list is dynamic:** a Muse Spark id that the gateway does not serve for an account answers `400 {"detail":"model_unavailable"}`. Listing it would only make agents pick a model that cannot answer, so the catalog is read from the account rather than hardcoded.

> **Connecting OmniRush:** OmniRush accounts can be connected in 1 click via the native CPA Management Dashboard (`/v0/resource/plugins/cpa-freetier-plugin/dashboard`), by pasting `auth.json` / a refresh token into the dashboard's import drawer, or by placing credentials in `~/.omnirush/auth.json` (auto-detected).

> **Wire protocol:** the gateway only accepts `POST <gateway>/v1/responses` with `stream: true` and `store: false`, and refuses requests that carry `x-omnirush-session-id` unless they come from the official CLI (`403 official_client_required`). The plugin therefore issues a plain Responses stream and translates it back to Chat Completions, which is why tool calls, reasoning summaries and usage all survive the round trip.

### 2. OpenCode Zen Frontier Models (10 Models)

| Model Identifier | Display Name in Code | Context Window | Max Output | Thinking |
| :--- | :--- | :--- | :--- | :---: |
| `flint-40b-free` | `Flint 40B Free (OpenCode)` | **262,144** | 65,536 | ✅ |
| `nemotron-3-ultra-free` | `Nemotron 3 Ultra Free (OpenCode)` | **1,000,000** | 128,000 | ✅ |
| `glm-5.3-flash-free` | `GLM 5.3 Flash Free (OpenCode)` | **200,000** | 32,768 | ✅ |
| `big-pickle` | `Big Pickle Free (OpenCode)` | **200,000** | 32,000 | ✅ |
| `deepseek-v4.1-flash-free` | `DeepSeek V4.1 Flash Free (OpenCode)` | **1,000,000** | 384,000 | ✅ |
| `mimo-v2.6-flash-free` | `MiMo v2.6 Flash Free (OpenCode)` | **262,144** | 65,536 | ✅ |
| `beam-free` | `Beam Free (OpenCode)` | **200,000** | 32,768 | ✅ |
| `nemotron-3.5-super-vl-free` | `Nemotron 3.5 Super VL Free (OpenCode)` | **1,000,000** | 65,536 | — |
| `cat-jam-free` | `Cat Jam Free (OpenCode)` | **1,048,576** | 131,072 | ✅ |
| `nemotron-3.5-lightning-free` | `Nemotron 3.5 Lightning Free (OpenCode)` | **1,000,000** | 65,536 | — |

> **Note on retired OpenCode IDs:** `minimax-m3.1-flash-free`, `exo-free`, `space-bunny-free`, `muse-spark-1.2-contributor-free`, and `muse-spark-1.3-contributor-free` were retired upstream (`410 ModelDeprecated` / `400 unknown model`). `longcat-2.5-preview-free` is unavailable upstream (`402` / timeout), and `jev-1.13-free` rejects chat completions (`ModelProtocolUnsupported`). Dormant `/inference/openai/v1/responses` routing for `muse-spark-1.2/1.3-contributor-free` remains in `callOpenCode` only for backwards compatibility if re-enabled upstream.

### 3. Kilo AI Curated Models (14 Models)

| Model Identifier | Display Name in Code | Context Window | Max Output | Thinking |
| :--- | :--- | :--- | :--- | :---: |
| `kilo-auto/free` | `Kilo Auto Free` | **256,000** | 32,768 | — |
| `nvidia/nemotron-3-ultra-550b-a55b:free` | `Nemotron 3 Ultra 550B Free (Kilo)` | **1,000,000** | 65,536 | ✅ |
| `cohere/north-mini-code:free` | `North Mini Code Free (Kilo)` | **256,000** | 64,000 | — |
| `openrouter/free` | `OpenRouter Free Auto (Kilo)` | **200,000** | 65,536 | — |
| `nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free` | `Nemotron 3 Nano Omni Free (Kilo)` | **256,000** | 65,536 | ✅ |
| `nvidia/nemotron-3.5-lightning:free` | `Nemotron 3.5 Lightning Free (Kilo)` | **1,000,000** | 65,536 | — |
| `nvidia/nemotron-3.5-super-vl:free` | `Nemotron 3.5 Super VL Free (Kilo)` | **1,000,000** | 131,072 | — |
| `poolside/laguna-s-2.1:free` | `Poolside Laguna S 2.1 Free` | **262,144** | 32,768 | — |
| `poolside/laguna-xs-2.1:free` | `Laguna XS 2.1 Free (Kilo)` | **262,144** | 32,768 | — |
| `dots-studio/dots-3-note-preview:free` | `Dots3-Note Preview Free (Kilo)` | **512,000** | 460,800 | — |
| `liquid/lfm-2.5-2.6b:free` | `LiquidAI LFM2.5 2.6B Free (Kilo)` | **65,536** | 8,192 | — |
| `reflection/beam-free` | `Reflection Beam Free (Kilo)` | **1,048,576** | 131,072 | — |
| `xingchen-agi/xing4.0-29b-a4b:free` | `Xingchen 4.0 29B Free (Kilo)` | **262,144** | 235,929 | — |
| `nvidia/nemotron-3.5-content-safety:free` | `Nemotron 3.5 Content Safety Free (Kilo)` | **128,000** | 8,192 | — |

> **Note on retired Kilo IDs:** `minimax/minimax-m3.1-flash:free`, `thinkingmachines/inkling-small:free`, and `nvidia/nemotron-3-super-120b-a12b:free` were removed upstream (`404 The requested model is currently unavailable`). `scripts/check-models.sh` and `scripts/probe-models.sh` guard against advertising dead IDs.

---

## 🚀 Installation & Setup

### Method 1: CLIProxyAPI Plugin Store (1-Click Install)

1. Open your CLIProxyAPI Management Center (`#/plugin-store`).
2. Search for **CPA Free-Tier Models** (`cpa-freetier-plugin`) and click **Install**.
3. Upon initialization (`cliproxy_plugin_init` / `plugin.register`), the plugin automatically creates `~/.cli-proxy-api/cpa-freetier-plugin.json`.
4. Restart CLIProxyAPI if prompted.

---

### Method 2: Manual Installation (All Platforms)

Download the verified platform package from [Releases](https://github.com/tsaQB/cpa-freetier-plugin/releases/latest). Each `.zip` archive contains the compiled shared library (`cpa-freetier-plugin.so` / `.dylib` / `.dll`) and its companion plugin manifest (`cpa-freetier-plugin.json`).

#### 🐧 Linux (x86_64 / amd64)
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-freetier-plugin/releases/download/v1.5.3/cpa-freetier-plugin_1.5.3_linux_amd64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

#### 🐧 Linux (ARM64 / aarch64)
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-freetier-plugin/releases/download/v1.5.3/cpa-freetier-plugin_1.5.3_linux_arm64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

#### 🍏 macOS (Apple Silicon M1/M2/M3/M4)
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-freetier-plugin/releases/download/v1.5.3/cpa-freetier-plugin_1.5.3_darwin_arm64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

#### 🍏 macOS (Intel x86_64)
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-freetier-plugin/releases/download/v1.5.3/cpa-freetier-plugin_1.5.3_darwin_amd64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

#### 🪟 Windows (x86_64)
Extract `cpa-freetier-plugin_1.5.3_windows_amd64.zip` into your configured `plugins/` directory so that `cpa-freetier-plugin.dll` and `cpa-freetier-plugin.json` reside inside `plugins/`.

#### Enable in `config.yaml` & Configure Descriptor
Enable the plugin in CLIProxyAPI's `config.yaml`:
```yaml
plugins:
  enabled: true
  dir: "plugins" # or full path to plugins directory
  configs:
    cpa-freetier-plugin:
      enabled: true
```

On startup, `ensureAuthDescriptor()` automatically writes `~/.cli-proxy-api/cpa-freetier-plugin.json` (and into `CPA_AUTH_DIR` / `AUTH_DIR` if those directories exist). You can optionally add `"excluded_models"` (or `"excluded-models"`) to hide models from `/v1/models` and block routing them upstream:
```json
{
  "disabled": false,
  "provider": "cpa-freetier-plugin",
  "type": "cpa-freetier-plugin",
  "excluded_models": [
    "muse*",
    "kilo-auto/free"
  ]
}
```

---

## 💡 How OpenCode Routing, Proxies & Sidecar Bypass Work

While **Kilo AI** models (`callKilo`) always connect directly through the host's standard network environment (`http.ProxyFromEnvironment`), **OpenCode Zen** free-tier models (`callOpenCode`) pass through upstream Cloudflare WAF and v2 gate validation.

### 1. Endpoint Resolution (`getOpenCodeBaseURL`)
On every OpenCode call, the plugin resolves the target endpoint in this exact order:
1. **`OPENCODE_BASE_URL` environment variable:** If set, requests are sent to the configured base URL (appending standard OpenAI `/v1/chat/completions` or `/v1/responses`, or respecting custom paths like `/inference/openai/v1`).
2. **Local Sidecar Auto-Detection (`127.0.0.1:8789`, required for OpenCode):** If `OPENCODE_BASE_URL` is unset, the plugin dials `127.0.0.1:8789` with a `50ms` TCP timeout. If a listener answers, requests are routed to `http://127.0.0.1:8789/v1/chat/completions` (or `/v1/responses` for responses models).
3. **Default Direct Upstream:** Otherwise, requests go directly to `https://opencode.ai/inference/openai/v1/chat/completions` (or `/inference/openai/v1/responses`).

> **OpenCode Sidecar Architecture (v1.5.3):**
> * **HTTP Layer (Native Go Harness):** The 8.2 KB agent preamble, official gate tools, W3C trace headers, SHA-1 project hashing, and candidate failovers are **compiled directly into the plugin binary** (`opencode_harness.go`).
> * **TLS Layer (Bun Sidecar Bridge):** OpenCode's Cloudflare front-end strictly filters TLS ClientHello / JA3 fingerprints matching the official Bun-compiled CLI. Direct Go `crypto/tls` connections are rejected with `HTTP 429 FreeUsageLimitError`. Running [`zen-proxy`](https://github.com/12errh/zen-proxy) on `127.0.0.1:8789` provides the necessary BoringSSL TLS bridge to pass gate inspection. Kilo AI and OmniRush remain 100% native zero-dependency.

### 2. Outbound Egress & Proxy Selection (`getOpenCodeProxy`)
When calling the remote `https://opencode.ai` upstream (`isLocal == false`), Cloudflare WAF blocks datacenter ASNs with `403 Forbidden` (`FreeTierError: OpenCode's free tier can only be used from within OpenCode`). The plugin selects outbound egress in this exact order:
1. **`OPENCODE_PROXY`:** Explicit proxy URL override (`http://`, `https://`, `socks5://`, `socks5h://`). Always wins if set.
2. **`OPENCODE_PROXY_POOL`:** Comma-separated list of candidate proxies (`host:port` or full URL; bare `host:port` defaults to `http://`). Empty by default (direct egress). Each candidate is tested via a `60ms` TCP dial, and the first reachable proxy is cached for `30s` (`proxyProbeTTL`).
3. **`OPENCODE_DATACENTER_PROXY_POOL` (Opt-in only):** Consulted **only** when `OPENCODE_ALLOW_DATACENTER` is enabled (`1`, `true`, `yes`, `on`). Defaults to `127.0.0.1:8123,127.0.0.1:8124,127.0.0.1:8125,127.0.0.1:8126`.
4. **Ambient / Direct Network Fallback:** If no pool candidate is reachable (or `OPENCODE_PROXY_POOL=""`), the plugin falls back to `http.ProxyFromEnvironment` (respecting `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`, or connecting direct).

### 3. Quick Setup by Environment

* **🏠 Laptop, Home PC, or Residential Broadband:**
  Works 100% direct out-of-the-box — no proxy is probed unless you configure one. To route through a local tunnel (for example a mobile/residential egress on `127.0.0.1:8127`):
  ```bash
  export OPENCODE_PROXY_POOL=""
  ```
* **☁️ Cloud VPS / Datacenter Host (AWS, Hetzner, DigitalOcean, etc.):**
  Provide a residential, mobile (4G/LTE), or SOCKS5 proxy so OpenCode does not return `403 FreeTierError`:
  ```bash
  export OPENCODE_PROXY="http://user:password@your-residential-proxy:8080"
  # or SOCKS5h:
  export OPENCODE_PROXY="socks5h://127.0.0.1:1080"
  # or a comma-separated fallback pool:
  export OPENCODE_PROXY_POOL="127.0.0.1:8127,proxy.example.net:3128"
  ```

---

## 🛠 Environment Variables

### OmniRush Accounts

`~/.cli-proxy-api/omnirush-accounts.json` holds every signed-in identity (`version: 2`). Each entry keeps
its own token pair, `enabled` flag, `lastUsed`, `lastError` and `cooldownUntil`.

- Rotation: least recently used first; a failing account is skipped for 45 s (5 min on auth failure).
- Failover is per request and bounded to three accounts, so a pool where everything is broken fails fast.
- `POST`-only shapes are unavailable on plugin resource routes, so add/remove/toggle travel as query params
  with the dashboard token header.

### Settings File

`~/.cli-proxy-api/cpa-freetier-settings.json` (`0600`) is written by the dashboard and wins over the
environment variables below. Delete it, or press **Reset** in the sheet, to fall back to them.

### Plugin Runtime Variables (`main.go`, `omnirush.go`, `management.go`)

| Variable | Default | Purpose |
| :--- | :--- | :--- |
| `OPENCODE_BASE_URL` | *(unset; auto-probes `127.0.0.1:8789`)* | Override the OpenCode base URL (routes to `/v1/chat/completions` or `/v1/responses` for sidecars like `zen-proxy:8789`, or preserves `/inference/openai/v1` for direct `opencode.ai`). If unset and `127.0.0.1:8789` is reachable within 50ms, uses `http://127.0.0.1:8789`; otherwise uses direct `https://opencode.ai`. |
| `OPENCODE_PROXY` | *(unset)* | Explicit proxy URL for remote OpenCode calls (`http://`, `https://`, `socks5://`, `socks5h://`). Wins over the pools below. |
| `OPENCODE_PROXY_POOL` | *(empty — direct)* | Comma-separated `host:port` or proxy URLs probed in order (60ms dial timeout, cached 30s). Set to `""` (empty) to disable probing. |
| `OPENCODE_ALLOW_DATACENTER` | `0` | Enable (`1`, `true`, `yes`, `on`) to allow falling back to `OPENCODE_DATACENTER_PROXY_POOL`. Off by default because OpenCode rejects datacenter ASNs with `403 FreeTierError`. |
| `OPENCODE_DATACENTER_PROXY_POOL` | `127.0.0.1:8123,127.0.0.1:8124,127.0.0.1:8125,127.0.0.1:8126` | Datacenter proxy candidates probed only when `OPENCODE_ALLOW_DATACENTER` is enabled. |
| `OMNIRUSH_AUTH_FILE` | `<home>/.omnirush/auth.json` | Explicit file path to stored OmniRush OAuth credentials. Also checks `<home>/.cli-proxy-api/omnirush-auth.json` as fallback. |
| `OMNIRUSH_DIR` | `<home>/.omnirush` | Directory containing OmniRush `auth.json`. |
| `OMNIRUSH_ACCESS_TOKEN` | *(unset)* | Direct Bearer access token override for OmniRush. If set, bypasses file reads. |
| `OMNIRUSH_REFRESH_TOKEN` | *(unset)* | Refresh token paired with `OMNIRUSH_ACCESS_TOKEN` for automatic rotation. |
| `OMNIRUSH_BASE_URL` | `https://omnirush.ai/omnirush` | Custom OmniRush gateway origin. The plugin appends `/v1` and calls `/v1/responses` and `/v1/models` on it. |
| `OMNIRUSH_PROXY` | *(unset)* | Proxy URL for outbound OmniRush OAuth and model calls. |
| `CPA_EXCLUDED_MODELS` | *(unset)* | Comma-separated list or JSON array (e.g. `muse*,kilo-auto/free` or `["muse*"]`) of model IDs/globs to hide from `/v1/models` and block with `404 model_not_found`. Combined with `"excluded_models"` / `"excluded-models"` from auth descriptor JSON files (cached 2s). |
| `CPA_STRIP_FIELDS` | *(unset)* | Extra comma-separated top-level JSON request fields to strip during payload sanitization. |
| `CPA_KEEP_FIELDS` | *(unset)* | Comma-separated top-level JSON request fields to keep even if present in `defaultSanitizeFields` or `CPA_STRIP_FIELDS`. Wins over `CPA_STRIP_FIELDS`. |
| `CPA_FREETIER_LOG` | `<home>/.cli-proxy-api/logs/cpa-freetier-plugin.log` | Log file path (`0600` permissions, rotates at `8 MiB` keeping one `.1` file). Falls back to `<os.TempDir()>/cpa_plugin.log` if home directory is unresolvable or the log directory cannot be created. |
| `CPA_FREETIER_DEBUG` | `0` | Enable (`1`, `true`, `yes`, `on`) for verbose diagnostic logging. Request bodies are **never** logged (only byte length and 8-byte SHA-256 hex prefix). |
| `CPA_NO_AUTH_DESCRIPTOR` | `0` | Enable (`1`, `true`, `yes`, `on`) to prevent `ensureAuthDescriptor()` from creating `cpa-freetier-plugin.json`. |
| `CLI_PROXY_AUTH_DIR` | *(unset)* | Additional auth directory scanned by `getAuthDirs()` for descriptor JSON files (`excluded_models`). |
| `CPA_AUTH_DIR` | *(unset)* | Additional auth directory used by both `ensureAuthDescriptor()` (writes `cpa-freetier-plugin.json` if dir exists) and `getAuthDirs()`. |
| `AUTH_DIR` | *(unset)* | Additional auth directory checked by `ensureAuthDescriptor()` when writing `cpa-freetier-plugin.json`. |

### Verification Script Variables (`scripts/`)

| Variable | Script | Default | Purpose |
| :--- | :--- | :--- | :--- |
| `KILO_MODELS_URL` | `check-models.sh` | `https://api.kilo.ai/api/gateway/models` | Upstream Kilo catalogue URL. |
| `ZEN_MODELS_URL` | `check-models.sh` | `https://opencode.ai/zen/v1/models` | Upstream OpenCode Zen catalogue URL. |
| `PROBE_PROXY` | `probe-models.sh` | *(unset)* | Comma-separated proxy URLs rotated across live model probe attempts. |
| `PROBE_ATTEMPTS` | `probe-models.sh` | `3` | Maximum attempts per model before reporting `INCONCLUSIVE`. |
| `PROBE_SLEEP` | `probe-models.sh` | `2` | Delay in seconds between sequential model probes. |
| `KILO_URL` | `probe-models.sh` | `https://api.kilo.ai/api/gateway/chat/completions` | Upstream Kilo chat completions endpoint for live probes. |

---

## 🩺 Verifying the Catalogue

Two verification scripts are included because catalogue presence does not guarantee liveness:

| Command | What it does |
| :--- | :--- |
| `make check-models` | Runs `./scripts/check-models.sh main.go`. Diffs `kiloModels` (fatal) and `openCodeModels` (warning) against live upstream catalogue endpoints (`kilo-auto/*` and `openrouter/*` router aliases are exempted). |
| `make probe-models PROBE_PROXY=http://127.0.0.1:8127` | Runs `./scripts/probe-models.sh main.go`. Sends live cloaked requests to every model in `kiloModels` and `openCodeModels` and classifies responses into `ALIVE` (`200`), `DEAD` (`404` / `410` / missing markers — fails with exit code 1), or `INCONCLUSIVE` (`401` / `403` / `429` / `5xx` / timeout). |
| `make verify` | Runs `go vet ./...`, `go test ./...`, and `./scripts/check-models.sh main.go`. |

---

## 🔒 Architecture & Reliability Details (v1.5.3)

* **Bounded, Privacy-Safe Logging:** Request bodies are never written to disk; only payload byte length and an 8-byte SHA-256 hex fingerprint (`payloadFingerprint`) are logged. The log file (`0600`) rotates at `8 MiB` (`maxLogBytes`) with one `.1` backup.
* **Per-Egress Connection Pooling:** `http.Transport` instances are cached process-wide per egress key (`MaxIdleConns: 128`, `MaxIdleConnsPerHost: 32`, `IdleConnTimeout: 90s`, `TLSHandshakeTimeout: 15s`, `ResponseHeaderTimeout: 120s`, `ExpectContinueTimeout: 1s`, `ForceAttemptHTTP2: false`). On remote transport errors, both the cached transport and proxy probe cache are invalidated immediately.
* **Bounded 16 MiB SSE Scanner:** Both streaming (`executeStream`) and non-streaming (`execute`) SSE readers allocate a 256 KiB initial buffer able to grow to `16 MiB` (`maxSSELineBytes`) per `data:` line so large tool-call arguments never abort with `bufio.ErrTooLong`.
* **2 GiB+ C-ABI Buffer Safety:** `cliproxyPluginCall` copies request bytes via `unsafe.Slice` over `size_t` instead of `C.GoBytes` (`C.int`), avoiding 32-bit length truncation.
* **Referral on sign-up:** every console link the dashboard hands out carries the configured referral (`identity.referralCode`, default `5WSC5CWX`, override with `OMNIRUSH_REFERRAL_CODE`), so a brand-new account credits the operator. The device-code query is preserved, so approving still works. The drawer exposes it only as a neutral "Register Now" button — no invite URL or code is rendered. Override with `OMNIRUSH_REFERRAL_CODE` or `-` to disable.
* **OmniRush Account Pool:** several OmniRush accounts can be signed in at once. They live in `~/.cli-proxy-api/omnirush-accounts.json` (`0600`, override with `OMNIRUSH_ACCOUNTS_FILE`); a legacy `~/.omnirush/auth.json` is migrated automatically on first run. Requests go to the least recently used healthy account, and a `401`, `429`, `402`, `403` or timeout benches that account for a cooldown and retries the next one — so one throttled identity no longer stalls the whole model list. Each account has its own single-flight refresh mutex, and rotations are mirrored back to the CLI credential file. The dashboard lists every account with its state (active / cooling down / disabled), a per-account toggle, and a remove action.
* **OmniRush Token Refresh:** `/device/refresh` is called on the host root, not on the API base (`gateway_url` ships as `https://omnirush.ai/omnirush/v1`, and `/v1/device/refresh` answers `401 device_token_required`, which reads like an expired token). Rotation is single-flight — concurrent 401s reuse the pair the first caller obtained because a refresh token is spend-once — and the rotated pair is written back to the file it was read from, so a co-installed OmniRush CLI never keeps a spent token. `409 refresh_token_already_used` adopts the peer's stored pair instead of demanding a new sign-in.
* **Settings Panel:** the gear in the dashboard header opens a sheet for egress and request behaviour — OpenCode proxy + pool, datacenter fallback, OmniRush proxy, CLI identity (user agent, client/project headers, pinned decoy session, sidecar URL), header timeout, retries, strip/keep fields and verbose logging. Values persist to `~/.cli-proxy-api/cpa-freetier-settings.json` (`0600`, override with `CPA_FREETIER_SETTINGS`) and take precedence over the matching environment variables, so nothing needs a restart. Every proxy field has a live **Test** button that reports TCP latency plus the HTTP status seen through it, and `-` disables a field explicitly.
* **Dashboard Mutation Token:** the host serves `/v0/resource/plugins/<id>/*` without the Management key, and every dashboard action is a GET, so the page is served with a per-process random token (`__CPA_FREETIER_TOKEN__` placeholder replaced at serve time) that `start`, `import`, `disconnect` and `models/toggle` must echo in `X-Cpa-Freetier-Token`. Without it those calls answer `403` and change nothing, which also blocks cross-site `<img src="…">` triggers.

---

## 🧪 Verification & Usage Example

Test your deployment via `curl` against your local CLIProxyAPI instance (`:8317`):

### Streaming Call
```bash
curl -N http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer your-cpa-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "flint-40b-free",
    "messages": [{"role": "user", "content": "Explain quantum tunneling in two sentences."}],
    "reasoning_effort": "medium",
    "stream": true
  }'
```

### Python OpenAI SDK
```python
import os
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8317/v1",
    api_key=os.getenv("CLIPROXY_API_KEY", "your-cpa-key"),
)

response = client.chat.completions.create(
    model="flint-40b-free",
    messages=[{"role": "user", "content": "Hello world!"}],
    stream=True,
)

for chunk in response:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
print()
```

---

## 🏗️ Building From Source

Prerequisites: **Go 1.22+** with `CGO_ENABLED=1` and platform GCC toolchains (`gcc`, `aarch64-linux-gnu-gcc`, `x86_64-w64-mingw32-gcc`). `pluginVersion` is stamped automatically from `git describe --tags` via `-ldflags "-s -w -X main.pluginVersion=$(VERSION)"`.

```bash
# Clone the repository
git clone https://github.com/tsaQB/cpa-freetier-plugin.git
cd cpa-freetier-plugin

# Build default Linux AMD64 shared library (dist/cpa-freetier-plugin_linux_amd64.so)
make build
# or: make build-linux-amd64 (alias: make build-amd64)

# Build Linux ARM64 shared library (dist/cpa-freetier-plugin_linux_arm64.so)
make build-linux-arm64
# (alias: make build-arm64)

# Build Windows AMD64 DLL (dist/cpa-freetier-plugin_windows_amd64.dll)
make build-windows-amd64
# (alias: make build-windows)

# Build all three targets (Linux AMD64, Linux ARM64, Windows AMD64)
make all

# Run go vet, unit tests, and upstream catalogue check
make verify
```

---

## 📄 License

Distributed under the **MIT License**. See `LICENSE` for more information.
