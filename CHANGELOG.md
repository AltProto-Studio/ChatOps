# Release History & Version Logs

<table align="center">
  <thead>
    <tr>
      <th align="center"><a href="README.md">🇺🇸 English</a></th>
      <th align="center"><a href="README.zh-CN.md">🇨🇳 简体中文</a></th>
      <th align="center"><a href="LICENSE.md">⚖️ License Audit</a></th>
      <th align="center">🟢 Releases</th>
    </tr>
  </thead>
</table>

---

All release binaries (Master & Agent for both Windows and Linux amd64) are compiled and attached to their respective Release tags on GitHub.  
所有平台的发布二进制包均已编译并挂载于 GitHub 对应的 Releases 页面中。

---

### 🟢 [v1.2.0] - Active Release / 当前活跃版本
* **New Features & Enhancements (新特性与改进)**:
  * **Interactive Update Confirmation (升级确认流交互改造)**: Integrated GitHub update checks in `⚙️ 更多功能` -> `🔍 检查更新`. Shows release body and prompts for manual `yes`/`no` keyboard inputs to confirm upgrading Master, removing obsolete inline buttons.
  * **Sleek Unicode Node Alias (别名支持中文与符号)**: Relaxed node alias requirements to support arbitrary Chinese, English, underscores, and hyphens (excluding whitespaces to prevent breaking command parsing).
  * **Auto Master IP Detection (Master 公网 IP 自动获取)**: Implements public IP retrieval (via `api.ipify.org` lookup) and local network interface IP fallback, providing a real IP recommended on the Telegram Reply Keyboard.
  * **Configuration Warning Fallback (配置文件缺失防崩溃)**: Replaced hard crash logs with a warning fallback when `master.yaml` or `agent.yaml` is missing, auto-generating template files and running with default values.
  * **Test Script Compilation Fixes (测试脚本编译修复)**: Fixed signature compile issues in `run-all` and `test-e2e` scripts.

---

### 🟡 [v1.1.0] - Node SSH Deployment Release
* **New Features & Enhancements (新特性与改进)**:
  * **Auto-SSH Node Addition (远程主机 SSH 自动化安装)**: Integrated `➕ 添加服务器` wizard, supporting remote Linux Agent installation via SSH stdin streams.
  * **Interactive Wizard Message Sweeper (向导密码毫秒级即发即删)**: Added dynamic message clean-up. Prompt bubbles and user password inputs are deleted instantly.
  * **gRPC TLS Safe Refactoring (自签名内存 TLS 证书)**: Supports dynamic ECDSA 10-year self-signed TLS certificates generated directly in the Master's memory.

---

### 🔴 [v1.0.0] - Initial Release
* **New Features & Enhancements (新特性与改进)**:
  * **gRPC Tunnel Core (双端 gRPC 反向长连接隧道)**: Bidirectional streaming RPC tunnel between Master and Agent with automated index-backoff reconnections.
  * **Telegram Command Core (电报机器人交互底层)**: Initial chatbot interface with `/invite`, `/join`, and `/deploy` command handlers.
  * **FIFO Build queues (FIFO 任务队列与负载超载熔断)**: Implements serialized task execution queues with CPU/Memory threshold interceptors.
  * **Caddy API routing (Caddy 自动代理与反向代理)**: Dynamic reverse proxy updates with fallbacks to file-based overrides.
