# CPA OpenCode Plugin

Native **CLIProxyAPI (ABI v1)** plugin untuk menyediakan model AI gratis tanpa login akun dari **OpenCode Zen**, **Xiaomi MiMo**, dan **Kilo AI**.

---

## 🌟 Fitur Utama

- **Zero-Login & Anti-403 Fingerprinting:**
  - Injeksi otomatis `Authorization: Bearer public`.
  - Emulasi resmi `User-Agent: opencode/1.18.31` dan `x-opencode-client: desktop`.
  - Generator sesi persisten canonical format `ses_<hex12><base62_14>` dan request header `msg_<hex12><base62_14>`.
  - Injeksi otomatis decoy tools quartet `{bash, glob, grep, read}` untuk bypass validasi upstream OpenCode.
  - Penegakan *forced streaming* (`stream: true`) untuk mencegah `403 FreeTierError`.
  - Routing khusus model `muse-spark-*` ke endpoint `/zen/v1/responses`.
- **Multi-Provider Bundled:**
  - **OpenCode Zen:** Muse Spark 1.3/1.2, DeepSeek V4 Flash, Nemotron 3.5, MiMo 2.5, Ling 3.0, Big Pickle.
  - **MiMo Free:** Auto bootstrap token JWT ke endpoint MiMo Code.
  - **Kilo AI:** Auto fallback free models (Poolside Laguna, Hunyuan 3, Step 3.7 Flash, Nemotron Ultra, dll.).
- **Dukungan Multi-Arsitektur:**
  - `linux/amd64` (x86_64)
  - `linux/arm64` (aarch64)

---

## 📋 Daftar Model yang Didukung

| Provider | Model ID | Tipe / Endpoint Upstream |
| :--- | :--- | :--- |
| **OpenCode Zen** | `muse-spark-1.3-contributor-free` | OpenAI Responses API (`/zen/v1/responses`) |
| **OpenCode Zen** | `muse-spark-1.2-contributor-free` | OpenAI Responses API (`/zen/v1/responses`) |
| **OpenCode Zen** | `deepseek-v4-flash-free` | Chat Completions (`/zen/v1/chat/completions`) |
| **OpenCode Zen** | `nemotron-3.5-lightning-free` | Chat Completions |
| **OpenCode Zen** | `nemotron-3-ultra-free` | Chat Completions |
| **OpenCode Zen** | `mimo-v2.5-free` | Chat Completions |
| **OpenCode Zen** | `ling-3.0-flash-fin-free` | Chat Completions |
| **OpenCode Zen** | `jev-1.13-free` | Chat Completions |
| **OpenCode Zen** | `big-pickle` | Chat Completions |
| **MiMo** | `mimo-auto` | MiMo Code Bootstrap + Chat |
| **Kilo AI** | `kilo-auto/free` | Kilo AI Gateway |
| **Kilo AI** | `tencent/hy3:free` | Kilo AI Gateway |
| **Kilo AI** | `stepfun/step-3.7-flash:free` | Kilo AI Gateway |
| **Kilo AI** | `nvidia/nemotron-3-ultra-550b-a55b:free` | Kilo AI Gateway |
| **Kilo AI** | `poolside/laguna-m.1:free` | Kilo AI Gateway |

---

## 🚀 Cara Pemasangan di CLIProxyAPI

### 1. Unduh Binary Plugin

Unduh binary `.so` sesuai arsitektur mesin dari [Halaman Rilis](https://github.com/tsaQB/cpa-opencode-plugin/releases):

**Untuk Linux ARM64 (aarch64):**
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-opencode-plugin/releases/download/v1.0.0/cpa-opencode-plugin_linux_arm64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

**Untuk Linux AMD64 (x86_64):**
```bash
mkdir -p ~/.cli-proxy-api/plugins
curl -sL https://github.com/tsaQB/cpa-opencode-plugin/releases/download/v1.0.0/cpa-opencode-plugin_linux_amd64.zip -o /tmp/plugin.zip
unzip -o /tmp/plugin.zip -d ~/.cli-proxy-api/plugins/
rm /tmp/plugin.zip
```

### 2. Konfigurasi `config.yaml` CLIProxyAPI

Tambahkan blok plugin pada file `/root/config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "/root/.cli-proxy-api/plugins"
  configs:
    cpa-opencode-plugin:
      enabled: true
```

### 3. Restart CLIProxyAPI

```bash
systemctl restart cliproxyapi
# atau jika dijalankan manual:
# kill -HUP <pid_cliproxyapi>
```

---

## 🧪 Pengujian Model

Gunakan endpoint OpenAI-compatible CLIProxyAPI (port default: `8317`):

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <API_KEY_CPA>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-flash-free",
    "messages": [{"role": "user", "content": "Halo! Siapa namamu?"}]
  }'
```

---

## 🛠️ Build dari Source (CI / GitHub Actions)

Karena plugin menggunakan CGO C ABI v1, kompilasi direkomendasikan melalui GitHub Actions:

- Buat Git Tag: `git tag v1.0.0 && git push origin v1.0.0`
- GitHub Actions akan otomatis melakukan cross-compile untuk `linux/amd64` dan `linux/arm64`, lalu membuat GitHub Release lengkap dengan file ZIP dan checksums.
