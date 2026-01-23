# Fulfillment Service 安全风险评估

## 1. 评估概述
本次评估针对 `fulfillment-service` 模块的代码实现、架构设计及数据流向进行。该模块负责核心资源的交付（Hosting VPS 和 VPN），涉及敏感的基础设施管理权限和用户访问凭据，因此具有极高的安全等级要求。

---

## 2. 身份验证与授权 (Authentication & Authorization)

### 2.1 内部服务通信 (Internal API)
- **风险描述**：内部接口（如 `/api/internal/*`）仅依赖 `X-Internal-Secret` 请求头进行硬编码令牌验证。
- **潜在影响**：如果该共享密钥泄露，攻击者可以伪造 `subscription-service` 的请求，非法置备、销毁或获取任何用户的资源。
- **改进建议**：
    - 引入动态令牌（如基于 mTLS 或内部短期 JWT）。
    - 密钥应通过环境变量或 Secret 管理服务进行轮转。

### 2.2 越权访问 (IDOR)
- **风险评估**：
    - **User API**：实现在 `handler.go` 中，正确使用了 JWT 中的 `userID` 作为过滤条件，风险较低。
    - **Internal API**：如 `GetResourceStatus` 仅根据 `resource_id` 查询，不校验用户归属。虽然它是内部接口，但若调用方（如 `subscription-service`）存在 IDOR，漏洞会透传至此。
- **建议**：在内部 API 响应中仅包含必要字段，并在关键操作逻辑中增加二次校验机制。

---

## 3. 敏感数据安全 (Sensitive Data Protection)

### 3.1 凭据存储 (Credentials Management)
- **风险描述**：
    - 数据库 `resources` 表存储了节点的 `APIKey`（用于连接 node-agent）和 VPN 的 `ss_password`。
    - `Resource` 模型中定义了 `SSHPrivateKey` 字段，且在 repository 中有对应的数据库操作。
- **潜在影响**：数据库泄露将导致用户节点被完全接管，或 SSH 权限被滥用。
- **改进建议**：
    - 敏感资产（如 SSH 私钥）应进行应用层加密后再入库，或使用 HashiCorp Vault 等专业 KMS 管理。
    - 数据库连接字符串 (`DB_URL`) 必须加密存储。

### 3.2 敏感信息日志泄露 (Log Leakage)
- **风险描述**：在 `HostingClient` 和 `VPNService` 中，错误发生时会将原始响应体（JSON）直接转字符串记录日志。
- **潜在影响**：如果外部服务返回包含 API 密钥、密码或其他敏感凭据，这些信息将被持久化到日志系统，增加泄露面。
- **建议**：在日志记录前，通过结构化日志过滤（Masking）或仅记录关键 ID 而非完整 Body。

---

## 4. API 安全与防御

### 4.1 输入验证
- **风险评估**：目前主要依赖 Gin 的 `binding:"required"` 进行基础校验。
- **风险点**：`region`、`plan_tier` 等字段缺乏严格的白名单校验，可能导致系统尝试创建不存在的资源类型，进而拖慢异步队列。
- **建议**：实现基于 Schema 的严格校验，对参数进行白名单过滤。

### 4.2 资源暴力破解与配额绕过
- **风险点**：用户创建节点的接口（`POST /my/node`）未见明显的速率限制（Rate Limiting）。
- **潜在影响**：可能遭遇恶意并发调用导致后端基础设施服务（Hosting Service）过载。
- **建议**：在 User API 层增加针对单用户的速率限制。

---

## 5. 基础设施安全 (Infrastructure)

### 5.1 Dockerfile 安全
- **评估点**：
    - 检查是否以 `root` 用户运行。
    - 基础镜像是否精简。
- **建议**：使用 `non-root` 用户运行容器镜像，减小攻击面。

---

## 6. 总结与路线图
`fulfillment-service` 的安全现状良好，采用了参数化查询防止 SQL 注入，且业务逻辑层面的用户隔离（Isolation）做得比较到位。

**高优先级任务：**
1.  **脱敏日志记录**：清理可能泄露 API 密钥的日志。
2.  **SSH 密钥脱离主库**：确保 SSH 私钥不以明文形式存储在通用业务库中。
3.  **速率限制**：为用户资源创建接口添加频率限制。
