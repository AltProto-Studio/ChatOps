# ChatOps - 极简无痕 Telegram 运维控制系统

<table align="center">
  <thead>
    <tr>
      <th align="center"><a href="README.md">🇺🇸 English</a></th>
      <th align="center">🟢 简体中文</th>
      <th align="center"><a href="LICENSE.md">⚖️ License Audit</a></th>
      <th align="center"><a href="CHANGELOG.md">🏷️ Releases</a></th>
    </tr>
  </thead>
</table>

---

## 📖 快速上手与使用指南

### 1. 控制端一键快捷安装 (Linux amd64 操作系统)
如果您希望快速在 Linux 服务器上将 Master 控制端作为系统守护进程运行，可直接执行以下一键安装脚本：
```bash
curl -fsSL https://raw.githubusercontent.com/AltProto-Studio/ChatOps/main/install-master.sh | sudo bash -s -- --token 您的TELEGRAM机器人TOKEN
```
*请将 `您的TELEGRAM机器人TOKEN` 替换为从 @BotFather 获取的真实 Token。该脚本将自动创建工作目录 `/opt/gopass/`、下载最新版二进制包、在本地生成配置文件 `master.yaml`、配置并激活 `gopass-master.service` Systemd 服务。*

---

### 2. 双端手动编译与配置运行

如果您需要从源码编译，或者在 Windows 等其他系统上运行：

#### 步骤 A：复制并修改配置模板
```bash
# 复制控制端（Master）配置
cp master.yaml.example master.yaml

# 复制被控端（Agent）配置
cp agent.yaml.example agent.yaml
```
* 编辑 `master.yaml` 并填入您的 `telegram_token`。
* 启动 Master 后控制台会生成一个安全通信密钥，将其拷贝并填入 `agent.yaml` 的 `communication_token` 中。

#### 步骤 B：本地编译
```bash
# 编译 Master 控制端
go build -o build/gopass-master.exe ./cmd/gopass-master

# 编译 Agent 被控端
go build -o build/gopass-agent.exe ./cmd/gopass-agent
```

#### 步骤 C：命令行启动
* 运行 Master：`./build/gopass-master.exe -config master.yaml`
* 运行 Agent：`./build/gopass-agent.exe -config agent.yaml`

---

### 3. Telegram 机器人使用流程

1. **首次绑定**：Master 服务首次启动后，第一个向 Bot 发送任何消息的 Telegram 账号将自动绑定为系统最高管理员。
2. **邀请与加入**：
   * 生成激活码：点击主菜单的 `🔑 生成邀请码` 或发送 `/invite [次数]`。
   * 子账号加入：未授权人员向 Bot 发送 `/join <激活码>` 绑定为普通子账户。
3. **SSH 远程自动化部署被控端**：
   * 在主菜单点击 `🖥️ 节点状态` -> `➕ 添加服务器`。
   * 按照提示输入目标机器 IP、SSH 端口、用户名、密码/私钥，以及 Master 的对外 IP。
   * 系统将自动完成跨平台交叉编译、安全上传、后台启动和 15 秒 gRPC 加密连通性校验。
4. **派发部署任务**：
   * 点击 `🚀 部署应用` -> 选择在线节点 -> 输入解析域名 -> 配置 Cloudflare DNS -> 选择 SSL 状态 -> 发送 Git 仓库地址。
5. **一键 GitHub 热更新**：
   * 点击 `⚙️ 更多功能` -> `🔍 检查更新`。
   * 系统将从 GitHub API 自动对比版本。若发现新版，展示更新日志并提示手动输入 `yes` 或 `no` 确认更新，回复 `yes` 即可自动无缝热替换 Master 二进制并优雅重启。

---

## 🚀 功能特性大体介绍

* **极简无痕交互**：首创“滚动气泡擦除”机制，历史向导过程气泡自动物理删除，用户发送的敏感密码/私钥毫秒级即发即删，绝不在聊天面板留下痕迹。
* **一键 SSH 部署**：免去 SFTP 依赖，通过 SSH 管道标准输入传输编译好的 Linux ELF 字节流，并进行自适应 Master 外部 IP 回连检测。
* **内存自签名 TLS 加密**：gRPC 控制信道全面实施 TLS 加密。支持在缺失物理证书文件时在内存中动态签发 ECDSA 证书，消除敏感密钥写盘风险。
* **超载熔断拦截**：为被控节点配备 FIFO 排队队列，若节点 CPU 占用超 85% 或内存超 90% 自动挂起队首任务，资源回落后自唤醒执行。
* **反代与 DNS 闭环**：全自动配置 Cloudflare DNS 纯解析或 CDN 代理，并协同 Agent 端 Caddy 代理服务器自适应申请 Let's Encrypt 证书。
