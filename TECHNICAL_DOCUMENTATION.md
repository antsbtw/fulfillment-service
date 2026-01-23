# Fulfillment Service 技术文档

## 1. 项目概述

`fulfillment-service` 是 SaaS 平台的核心组件之一，负责资源的生命周期管理和实际交付（Fulfillment）。它作为业务逻辑（订阅）与底层基础设施（VPS 托管、VPN 管理器）之间的桥梁，确保用户购买的服务能够被正确地初始化、监控和销毁。

### 1.1 主要职责
- **资源交付**：接收订阅服务的指令，创建 VPS 节点或 VPN 用户。
- **状态管理**：维护资源的实时状态（创建中、活跃、失败、已删除）。
- **流程编排**：协调 `hosting-service`（基础设施）和 `subscription-service`（业务状态）。
- **用户接口**：为前端提供用户拥有的节点和 VPN 状态查询及简单管理。

---

## 2. 系统架构

### 2.1 技术栈
- **语言**：Go 1.23+
- **框架**：Gin Web Framework
- **数据库**：PostgreSQL (使用 pgx 驱动)
- **认证**：JWT (用户接口) & Shared Secret (内部接口)

### 2.2 核心流程：Hosting Node 交付
1. `subscription-service` 发起 `POST /api/internal/provision`。
2. `fulfillment-service` 创建本地资源记录，状态设为 `pending`。
3. 异步调用 `hosting-service` 创建 VPS 实例。
4. 轮询 `hosting-service` 或接收回调等待 VPS 准备就绪。
5. VPS 上的 `node-agent` 自动安装服务并回调 `GET /api/callback/node/ready`。
6. `fulfillment-service` 更新资源为 `active` 并回传详细配置给 `subscription-service`。

---

## 3. API 接口规范

### 3.1 鉴权说明
- **User API** (`/api/v1/*`): 需要 `Authorization: Bearer <JWT_TOKEN>`。
- **Internal API** (`/api/internal/*`): 需要自定义头部鉴权（通常是 API Key）。
- **Public API** (`/api/v1/public/*`): 无需鉴权。

### 3.2 用户接口 (用于前端接入)

#### 1. 获取我的节点状态
- **Endpoint**: `GET /api/v1/my/node`
- **说明**: 返回当前用户的 Hosting 订阅及关联的节点信息。前端应根据 `hosting_status` 字段决定 UI 展示。
- **响应示例**:
```json
{
  "hosting_status": "node_active",
  "has_subscription": true,
  "subscription": {
    "subscription_id": "sub_123",
    "status": "active",
    "plan_tier": "premium",
    "expires_at": "2026-10-22T21:55:00Z"
  },
  "has_node": true,
  "node": {
    "resource_id": "res_456",
    "region": "ap-northeast-1",
    "status": "active",
    "public_ip": "1.2.3.4",
    "traffic_limit_gb": 1000,
    "traffic_used_gb": 12.5,
    "traffic_percent": 1.25
  },
  "message": "Node is active and ready to use."
}
```

#### 2. 创建我的节点
- **Endpoint**: `POST /api/v1/my/node`
- **Request Body**:
```json
{
  "region": "ap-northeast-1"
}
```
- **说明**: 用户有订阅但无节点时，调用此接口开始创建。

#### 3. 删除我的节点
- **Endpoint**: `DELETE /api/v1/my/node`
- **说明**: 销毁当前节点。通常用于节点异常需要重新创建的情况。

#### 4. 获取 VPN 状态
- **Endpoint**: `GET /api/v1/my/vpn`
- **响应示例**:
```json
{
  "vpn_status": "active",
  "has_vpn_user": true,
  "vpn_user": {
    "vpn_user_id": "user_789",
    "status": "active",
    "traffic_limit_gb": 500,
    "traffic_used_gb": 100
  }
}
```

#### 5. 获取区域列表
- **Endpoint**: `GET /api/v1/regions`
- **说明**: 获取可供创建节点的地理区域列表。

---

## 4. 数据模型 (Resource)

资源模型存储在 `resources` 表中，关键字段如下：

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `id` | UUID | 资源唯一标识 |
| `subscription_id` | String | 关联的订阅 ID |
| `user_id` | String | 所属用户 ID |
| `resource_type` | String | `hosting_node`, `vpn_user` |
| `status` | String | `creating`, `active`, `failed`, `deleted` |
| `region` | String | 云服务商区域代码 (如 `us-east-1`) |
| `public_ip` | String | 节点的公网 IP (仅限 hosting_node) |
| `traffic_limit` | BigInt | 流量限制 (Bytes) |
| `traffic_used` | BigInt | 已用流量 (Bytes) |

---

## 5. 前端集成建议

### 5.1 状态机处理
前端在展示 "我的节点" 页面时，应重点关注 `hosting_status`：
- `no_subscription`: 引导用户购买订阅。
- `subscribed_no_node`: 显示 "创建节点" 按钮。
- `node_creating`: 显示进度条（使用 `creation_progress` 对象中的步骤信息）。
- `node_active`: 显示节点详细配置、连接信息及控制面板。
- `node_failed`: 显示错误信息及 "删除并重试" 按钮。

### 5.2 实时性
虽然 `fulfillment-service` 暂时没有实现 WebSocket，但建议前端在 `node_creating` 状态下使用**指数退避算法进行轮询**（如每 5 秒、10 秒、20 秒请求一次），直到状态变为 `active` 或 `failed`。

---

## 6. 未来迭代导向

1. **自动迁移**：支持资源在不同区域间的平滑迁移。
2. **多节点支持**：目前的逻辑偏向于 "一个订阅一个节点"，未来可扩展为支持集群。
3. **监控告警集成**：集成 Prometheus 指标，不仅监控流量，还监控节点负载。
4. **WebSocket 回调**：在资源就绪时通过 WebSocket 主动通知前端，消除轮询开销。

---

## 7. 部署与环境配置

主要环境变量（详见 `.env.example`）：
- `DB_URL`: PostgreSQL 连接字符串。
- `JWT_SECRET`: 与 `auth-service` 共享的 JWT 密钥。
- `INTERNAL_SECRET`: 内部 API 鉴权密钥。
- `HOSTING_SERVICE_URL`: `obox-hosting-service` 的访问地址。
- `SUBSCRIPTION_SERVICE_URL`: `subscription-service` 的访问地址。
