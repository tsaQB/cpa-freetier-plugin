# CPA Free-Tier Plugin

<p align="center">
  <img src="https://img.shields.io/badge/CLIProxyAPI-ABI%20v1%20Compliant-00ADD8?style=flat-square&logo=go" alt="CPA ABI v1" />
  <img src="https://img.shields.io/badge/Architecture-AMD64%20%7C%20ARM64-4A154B?style=flat-square" alt="Arch AMD64 / ARM64" />
  <img src="https://img.shields.io/badge/License-MIT-green?style=flat-square" alt="License MIT" />
</p>

A native dynamic C ABI plugin (`.so`) for **[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** (v7+) providing curated, **100% free AI models** without any API keys, accounts, or subscriptions.

---

## 🌟 Features

- **Zero API Keys:** Works out-of-the-box without login or setup.
- **Universal Compatibility:** Automatically cleans parameters (`reasoning_effort`, `thinking`) sent by agents like Hermes, Cursor, and Claude Code.
- **Native Streaming:** Clean SSE framing (`host.stream.emit`) with zero buffering.
- **Multi-Architecture:** Pre-compiled native binaries for Linux `amd64` and `arm64`.

---

## 📋 Active Models

| Model ID | Upstream | Notes |
| :--- | :--- | :--- |
| `nvidia/nemotron-3-ultra-550b-a55b:free` | Kilo AI | 550B flagship reasoning model |
| `nvidia/nemotron-3-super-120b-a12b:free` | Kilo AI | 120B high-context model |
| `nvidia/nemotron-3.5-lightning:free` | Kilo AI | Nemotron 3.5 lightning-fast model |
| `stealth/space-bunny-alpha` | Kilo AI | 1M ultra-long context frontier model |
| `stepfun/step-3.7-flash:free` | Kilo AI | Fast reasoning & chat model |
| `inclusionai/ling-3.0-flash-fin:free` | Kilo AI | Financial & code reasoning specialist |
| `dots-studio/dots-3-note-preview:free` | Kilo AI | 512k context summarization specialist |
| `poolside/laguna-s-2.1:free` | Kilo AI | Code generation specialist |
| `liquid/lfm-2.5-2.6b:free` | Kilo AI | Liquid Neural Network model |
| `nex-agi/nex-n2.5-pro:free` | Kilo AI | Agentic & multi-turn model |
| `nex-agi/nex-n2.5-mini:free` | Kilo AI | Ultra-low latency model |
| `cohere/north-mini-code:free` | Kilo AI | Fast syntax & code helper |
| `kilo-auto/free` | Kilo AI | Auto-routes to healthy free pool |
| `openrouter/free` | Kilo AI | Dynamic community fallback pool |

---

## 🚀 Quick Install

### 1. Download Binary

Choose the command matching your architecture:

**Linux ARM64 (aarch64):**
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-freetier-plugin/releases/download/v1.0.0/cpa-freetier-plugin_linux_arm64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

**Linux AMD64 (x86_64):**
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-freetier-plugin/releases/download/v1.0.0/cpa-freetier-plugin_linux_amd64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

### 2. Enable in `config.yaml`

Add this block to your CLIProxyAPI `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "/root/.cli-proxy-api/plugins"
  configs:
    cpa-freetier-plugin:
      enabled: true
```

### 3. Restart CLIProxyAPI

```bash
systemctl restart cliproxyapi
```

---

## 🧪 Usage Example

Point any OpenAI-compatible client to port `8317`:

```bash
curl -N http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer YOUR_CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "nvidia/nemotron-3-ultra-550b-a55b:free",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

---

## 📄 License

MIT

