# ChatOps - 极简无痕 Telegram 运维控制系统

ChatOps 是一个基于 Go 开发的极简无痕 Telegram 运维自动化控制系统。它包含两个主要组件：
- **Master (控制端)**：接收 Telegram Bot 指令、协调并下发部署任务、维护集群状态。
- **Agent (被控端)**：接收部署指令，自动拉取源码、编译镜像、运行容器并自动刷新 Caddy 反向代理路由。

---

## 🚀 项目特性

- **无痕交互**：Telegram 交互采用“原地消息编辑与物理擦除”机制，保证控制台干净整洁。
- **并发控制**：内置基于 FIFO 的节点任务队列，保证多任务部署顺序执行，避免争抢。
- **动态防爆**：自带资源拦截器，若 Agent 节点的 CPU 超过 85% 或内存超过 90%，将自动拦截并挂起任务，待资源恢复后自动唤醒。
- **自动反代与 SSL**：深度对接 Caddy 反向代理，支持动态绑定 SSL 与 Caddy 自动证书触发。
- **Cloudflare DNS 自动化**：支持对接 Cloudflare API，自动为部署的应用配置 CNAME / A 记录。

---

## 🛠️ 外部依赖与工具下载引导

项目涉及 `.proto` 协议文件的编译，在重新生成协议代码时需要 `protoc` 工具。**本代码库不打包分发该外部二进制程序。**

### Protocol Buffers Compiler (protoc) 下载引导

如果您需要重新编译 `pkg/proto/protocol.proto` 协议，请按以下步骤配置 `protoc`：

1. **下载编译器**：
   - 前往 [Protocol Buffers Releases](https://github.com/protocolbuffers/protobuf/releases) 页面。
   - 下载适用于您操作系统的压缩包。例如 Windows x64 下载 `protoc-X.Y.Z-win64.zip`，macOS 下载 `protoc-X.Y.Z-osx-aarch_64.zip`。

2. **配置环境变量**：
   - 解压下载的压缩包。
   - 将解压出的 `bin` 目录路径（内含 `protoc` 或 `protoc.exe` 执行文件）添加到您系统的 `PATH` 环境变量中。

3. **安装 Go 插件**（在命令行中运行）：
   ```bash
   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
   ```

4. **编译协议文件**（若修改了 `.proto` 文件）：
   在项目根目录下执行：
   ```bash
   protoc --go_out=. --go-grpc_out=. pkg/proto/protocol.proto
   ```

---

## 📦 部署与运行

### 1. 配置准备

项目中包含了配置文件模板。请在启动前复制并修改它们：

```bash
# 复制控制端配置模板
cp master.yaml.example master.yaml

# 复制被控端配置模板
cp agent.yaml.example agent.yaml
```

**配置说明**：
- **master.yaml**: 填入您的 Telegram Bot Token。
- **agent.yaml**: 填入 Master 端的地址，并配置 Master 启动时生成的 `communication_token`（通信密钥）。

### 2. 编译并运行

#### 运行 Master 端：
```bash
# 编译 Master 二进制
go build -o gopass-master ./cmd/gopass-master

# 启动 Master 端
./gopass-master -config master.yaml
```
*注：首次启动 Master 时，控制台将输出生成的 `Communication Token`。同时，第一个向 Bot 发送任何消息的 Telegram 用户将被自动绑定为最高管理员。*

#### 运行 Agent 端：
```bash
# 编译 Agent 二进制
go build -o gopass-agent ./cmd/gopass-agent

# 启动 Agent 端
./gopass-agent -config agent.yaml
```

---

## 🔒 安全说明

> [!WARNING]
> 本项目的 gRPC 长连接底层默认使用明文传输 (`insecure.NewCredentials()`)。在生产/公开网络中部署时，强烈建议配置 TLS 证书。您可修改 `pkg/agent/client.go` 和 `pkg/master/server.go` 的连接配置来加载您的 SSL/TLS 证书。
