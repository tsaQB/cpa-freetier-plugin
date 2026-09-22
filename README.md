# CPA OpenCode & Free Models Plugin

<p align="center">
  <img src="https://img.shields.io/badge/CLIProxyAPI-ABI%20v1%20Compliant-00ADD8?style=for-the-badge&logo=go" alt="CPA ABI v1" />
  <img src="https://img.shields.io/badge/Architecture-AMD64%20%7C%20ARM64-4A154B?style=for-the-badge" alt="Arch AMD64 / ARM64" />
  <img src="https://img.shields.io/badge/License-MIT-green?style=for-the-badge" alt="License MIT" />
  <img src="https://img.shields.io/badge/Build-GitHub%20Actions%20CI-blue?style=for-the-badge&logo=githubactions" alt="CI Status" />
</p>

A high-performance, native dynamic C ABI plugin (`.so`) for **[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** (v7+). Seamlessly exposes curated free and high-performance AI models from multiple upstream providers—including **Kilo AI Gateway**, **NVIDIA NIM**, and **OpenCode Zen**—without requiring individual client accounts, subscriptions, or complex auth setups.

---

## 🌟 Key Features

### 🛡️ Universal Client Compatibility & Payload Normalization
- **Strict Parameter Sanitization:** Automatically strips conflicting or upstream-rejected parameters (e.g. `reasoning_effort`, `reasoning.effort`, nested `thinking` blocks) emitted by agent runtimes like **Hermes Agent**, **Claude Code**, or **Cursor**.
- **Double SSE De-Framing:** Intelligently normalizes nested Server-Sent Events streams (fixing `data: data: {...}` formatting issues) before forwarding to the host proxy.
- **Native ABI v1 Streaming:** Implements official `host.stream.emit` and `host.stream.close` methods for zero-buffer real-time token streaming.

### ⚡ Smart Multi-Upstream Execution
- **Kilo AI Public Gateway:** Anonymous rate-safe routing to verified free models with automatic upstream request headers.
- **Direct NVIDIA NIM Integration:** Embedded handling for high-speed flagship models (e.g. `deepseek-ai/deepseek-v4.1-flash`) with internal reasoning cleanup.
- **Anti-403 Fingerprinting for OpenCode:** Dynamic canonical session generation (`ses_<hex12><base62_14>`), desktop user-agent cloaking, and tool quartet simulation.

### 🏗️ Enterprise-Grade CGO Build
- Distributed as zero-dependency native shared objects (`.so`) cross-compiled using GitHub Actions runners for both `linux/amd64` and `linux/arm64` (aarch64).

---

## 📋 Verified & Active Models Catalog

All models below are actively verified and ready for production use via `/v1/chat/completions`:

| Model Identifier | Upstream Provider | Characteristics & Use Cases |
| :--- | :--- | :--- |
| `nvidia/nemotron-3-ultra-550b-a55b:free` | **NVIDIA** | **550B Reasoning Flagship.** Heavyweight reasoning model with deep thought traces, ideal for complex architecture and coding. |
| `nvidia/nemotron-3-super-120b-a12b:free` | **NVIDIA** | **120B High-Context.** Ultra-reliable model with up to 1M context window for long documents and codebases. |
| `deepseek-ai/deepseek-v4.1-flash` | **NVIDIA NIM** | **DeepSeek V4.1 Flash.** Cutting-edge speed and reasoning accuracy with automated sanitization. |
| `poolside/laguna-s-2.1:free` | **Poolside AI** | **Code Specialist.** Highly accurate software engineering model built by Poolside. |
| `nex-agi/nex-n2.5-pro:free` | **Nex AGI** | **Agentic Pro.** Multi-turn tool use, function execution, and agentic reasoning workflows. |
| `nex-agi/nex-n2.5-mini:free` | **Nex AGI** | **Low Latency.** Sub-second response times for chat and fast lookups. |
| `stepfun/step-3.7-flash:free` | **StepFun** | **Step 3.7 Flash.** Native thinking trace model with superior natural language understanding. |
| `cohere/north-mini-code:free` | **Cohere** | **Compact Code.** Fast syntax analysis and code refactoring. |
| `kilo-auto/free` | **Kilo Router** | **Dynamic Load Balancer.** Automatically selects the best healthy free model pool. |
| `openrouter/free` | **OpenRouter** | **Public Pool Fallback.** Routes dynamically across available free community nodes. |

---

## 🚀 Installation & Setup

### 1. Download Binary Release

Download the pre-compiled `.so` library matching your server architecture from [Releases](https://github.com/tsaQB/cpa-opencode-plugin/releases/latest):

#### For Linux ARM64 (aarch64 - SBC, Termux, Cloud ARM):
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-opencode-plugin/releases/download/v1.0.0/cpa-opencode-plugin_linux_arm64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

#### For Linux AMD64 (x86_64 - VPS, Server, Desktop):
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-opencode-plugin/releases/download/v1.0.0/cpa-opencode-plugin_linux_amd64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

### 2. Configure CLIProxyAPI

Edit your CLIProxyAPI configuration file (e.g. `/root/config.yaml` or `~/.cli-proxy-api/config.yaml`) to enable plugin discovery:

```yaml
plugins:
  enabled: true
  dir: "/root/.cli-proxy-api/plugins"
  configs:
    cpa-opencode-plugin:
      enabled: true
```

### 3. Restart Service

Restart your CLIProxyAPI systemd service or reload the binary process:

```bash
systemctl restart cliproxyapi
# Or via CLIProxyAPI Management / CLI:
# cliproxyapi restart
```

---

## 🧪 Verification & Usage Examples

Once installed, point any OpenAI-compatible client (Hermes Agent, Xiao, Cursor, Claude Code, Python SDK, or cURL) to your local proxy port (default `8317`):

### cURL (Streaming Chat Request)
```bash
curl -N http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer YOUR_CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "nvidia/nemotron-3-ultra-550b-a55b:free",
    "messages": [
      {"role": "user", "content": "Explain how epoll works in the Linux kernel in 3 bullet points."}
    ],
    "stream": true
  }'
```

### Python (OpenAI SDK with Hermes Agent Rigor)
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8317/v1",
    api_key="YOUR_CPA_KEY"
)

response = client.chat.completions.create(
    model="deepseek-ai/deepseek-v4.1-flash",
    messages=[
        {"role": "system", "content": "You are a concise engineering assistant."},
        {"role": "user", "content": "Write an efficient LRU Cache in Go."}
    ],
    stream=True
)

for chunk in response:
    content = chunk.choices[0].delta.content or ""
    print(content, end="", flush=True)
print()
```

---

## 🛠️ Architecture & Protocol Specifications

```
                     ┌──────────────────────────────────────────────┐
                     │ Client Request (Hermes / Cursor / cURL)      │
                     └──────────────────────┬───────────────────────┘
                                            │ OpenAI-compatible REST / SSE
                                            ▼
                     ┌──────────────────────────────────────────────┐
                     │ CLIProxyAPI Host Process (Port 8317)         │
                     └──────────────────────┬───────────────────────┘
                                            │ Native C ABI v1 Calls
                                            ▼
┌───────────────────────────────────────────────────────────────────────────────────┐
│ cpa-opencode-plugin.so                                                            │
│                                                                                   │
│  ├── [Payload Sanitizer]       Strips conflicting reasoning_effort & thinking     │
│  ├── [Dynamic Model Catalog]   Advertises 100% active verified model identifiers  │
│  ├── [SSE Stream Re-Framer]    Cleans double 'data:' prefixes & emits via host    │
│  └── [Multi-Route Dispatcher]  Directs traffic to Kilo, NVIDIA NIM, or OpenCode   │
└──────────────┬────────────────────────────┬───────────────────────────┬───────────┘
               │                            │                           │
               ▼                            ▼                           ▼
   ┌───────────────────────┐   ┌─────────────────────────┐   ┌──────────────────────┐
   │   Kilo AI Gateway     │   │     NVIDIA NIM API      │   │    OpenCode Zen      │
   │  (Anonymous Free Tier)│   │  (Optimized Fast Path)  │   │  (Anti-403 Cloaked)  │
   └───────────────────────┘   └─────────────────────────┘   └──────────────────────┘
```

---

## 🤝 Contributing & Local Compilation

Local compilation requires Go 1.22+ with `CGO_ENABLED=1` and GCC/musl tooling:

```bash
git clone https://github.com/tsaQB/cpa-opencode-plugin.git
cd cpa-opencode-plugin

# Compile for local architecture:
make build

# Or cross-compile via Makefile targets:
make build-amd64
make build-arm64
```

*Note: For low-resource devices (e.g. ARM TV boxes / SBCs), always offload compilation to GitHub Actions CI workflows (`.github/workflows/build-and-release.yml`).*

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).

