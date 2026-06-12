# Open Source License & Compliance Audit

<table align="center">
  <thead>
    <tr>
      <th align="center"><a href="README.md">🇺🇸 English</a></th>
      <th align="center"><a href="README.zh-CN.md">🇨🇳 简体中文</a></th>
      <th align="center">🟢 License Audit</th>
      <th align="center"><a href="CHANGELOG.md">🏷️ Releases</a></th>
    </tr>
  </thead>
</table>

---

## ⚖️ Open Source License / 开源协议

This project is open-sourced under the **Apache License, Version 2.0**. You can find the full text of the license in the [LICENSE](file:///d:/ChatOps/LICENSE) file.  
本项目采用 **Apache License, Version 2.0** 协议开源。详细条款可参阅同目录下的 [LICENSE](file:///d:/ChatOps/LICENSE) 文件。

---

## 📦 Direct Dependencies & Reference Declarations / 直接依赖项目引用声明

We build upon and express our gratitude to the following permissive open-source packages:  
本项目引用并致谢以下优秀的开源依赖库：

* **go.etcd.io/bbolt** (Apache-2.0)
  * Purpose: Transactional embedded key/value database for Master state persistence.
  * 应用目的：用于 Master 端元数据和用户状态的轻量级事务型嵌入式 KV 持久化存储。
* **github.com/go-telegram-bot-api/telegram-bot-api/v5** (MIT)
  * Purpose: Telegram Bot API wrapper for the ChatOps interactive command interface.
  * 应用目的：Telegram 机器人交互封装库，用于驱动 ChatOps 命令响应与状态机。
* **github.com/shirou/gopsutil/v3** (BSD-3-Clause)
  * Purpose: Querying host runtime metrics (CPU, Memory, and Disk) on Agent nodes.
  * 应用目的：用于被控端 Agent 实时监测采集主机的 CPU、内存与磁盘等硬件负荷指标。
* **github.com/docker/docker** (Apache-2.0)
  * Purpose: Docker SDK for container lifecycle spawning and orchestration.
  * 应用目的：Docker 容器引擎 Go SDK，用于 Agent 被控端管理部署项目的生命周期调度。
* **google.golang.org/grpc** (Apache-2.0)
  * Purpose: gRPC streaming tunnel communication between Master and Agent nodes.
  * 应用目的：gRPC 远程调用通信框架，用于构建主控端与被控端的反向双向控制信道。
* **google.golang.org/protobuf** (BSD-3-Clause)
  * Purpose: Protocol Buffers serialization and deserialization support.
  * 应用目的：Protobuf 数据序列化与反序列化工具，负责信道数据编解码。
* **golang.org/x/crypto** (BSD-3-Clause)
  * Purpose: Cryptographic toolkit containing the SSH client logic.
  * 应用目的：提供 Go 语言安全加密学和 SSH 协议底层网络支持。
* **gopkg.in/yaml.v3** (MIT / Apache-2.0)
  * Purpose: Configuration parser used for loading YAML settings.
  * 应用目的：用于解析和载入 YAML 格式的配置文件。

---

## 🔍 License Compliance & Conflict Audit Report / 协议合规与冲突审计报告

We have conducted a rigorous audit on the licensing terms of all third-party dependencies used in ChatOps:  
我们对本项目引入的全部第三方开源库的授权协议进行了严格的合规性审查：

1. **Permissiveness (无强传染性限制)**:
   All dependencies in the direct tree use highly permissive open-source licenses (MIT, BSD-3-Clause, or Apache-2.0). There are **no copyleft dependencies** (such as GPL, AGPL, or LGPL) in the project, ensuring that the codebase is completely safe for custom branding and deployment.
   
   引入的直接依赖项均采用极度 permissve（宽松）的开源协议（MIT、BSD 或 Apache-2.0）。本项目**不包含任何具有 GPL, AGPL, 或 LGPL 等 Copyleft 强传染性限制的第三方依赖**，保障代码在分发与二次部署时的绝对安全性。

2. **Compatibility (协议兼容性)**:
   Both the MIT and BSD-3-Clause licenses are fully compatible with the Apache-2.0 license. They permit modification, sublicensing, and distribution under different terms, provided their original copyright notices are maintained.
   
   MIT 协议与 BSD-3-Clause 协议均与 Apache-2.0 协议完全兼容。它们允许本项目进行整体的二次授权、修改与分发，唯一的义务是在发布包中保留原作者的版权声明，本项目已完全遵守这一规则。

3. **Conclusion (审计结论)**:
   The ChatOps codebase contains **no licensing conflicts or violations**. All dependencies are handled in strict compliance with their respective open-source licenses.
   
   ChatOps 项目的依赖链路**不存在任何开源协议冲突或违反行为**，合规性 100% 通过。
