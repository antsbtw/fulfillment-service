package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// computeConfigVersion 算 config_version = {protocols, smart_strategy} 的稳定 hash（§8.3）。
//
// ★遵防 churn 铁律（§5.2，两次历史断流事故的教训 [[project_standard_node_reload_churn]]）：
//   - 只纳"变了才需前端刷新"的字段：protocols 的 {protocol, url, node} + smart_strategy 整体。
//     ★不纳 traffic_used / traffic_limit / expire_at 等——它们随用量/时间漂移，纳入则前端每次拉都变 →
//     无谓静默重连（等价 realm version churn 断流）。
//   - protocols 集合【排序】后再 hash：下发/组装行序漂移不得改变 version（顺序维度防护）。
//   - smart_strategy 是 otun-manager 下发的 RawMessage：再 Unmarshal→Marshal 归一，消除
//     上游 JSON 空白/键序差异带来的伪变化（RawMessage 原始字节可能因序列化实现不同而抖动）。
//
// 返回格式 "v<hex16>"（sha256 前 8 字节），与 otun-manager realm version hash 风格一致。
func computeConfigVersion(protocols []models.VPNProtocol, smartStrategy json.RawMessage) string {
	// protocols 稳定投影：只取 {protocol, url, node}，按三元组排序。
	type stableProto struct {
		Protocol string `json:"protocol"`
		URL      string `json:"url"`
		Node     string `json:"node"`
	}
	sp := make([]stableProto, len(protocols))
	for i, p := range protocols {
		sp[i] = stableProto{Protocol: p.Protocol, URL: p.URL, Node: p.Node}
	}
	sort.Slice(sp, func(i, j int) bool {
		if sp[i].Node != sp[j].Node {
			return sp[i].Node < sp[j].Node
		}
		if sp[i].Protocol != sp[j].Protocol {
			return sp[i].Protocol < sp[j].Protocol
		}
		return sp[i].URL < sp[j].URL
	})

	// smart_strategy 归一：Unmarshal 到通用 map 再 Marshal（Go json.Marshal 对 map 键按字典序稳定输出），
	// 消除上游原始字节的空白/键序差异。空/非法则以空对象参与（不影响标准分支——它本就无 smart_strategy）。
	var normStrategy interface{}
	if len(smartStrategy) > 0 {
		if err := json.Unmarshal(smartStrategy, &normStrategy); err != nil {
			// 非法 JSON：退回原始字节，保证仍产出确定 hash（不 panic、不空）。
			normStrategy = string(smartStrategy)
		}
	}

	payload := struct {
		Protocols     []stableProto `json:"protocols"`
		SmartStrategy interface{}   `json:"smart_strategy"`
	}{sp, normStrategy}

	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("v%s", hex.EncodeToString(sum[:8]))
}

// combineConfigVersions 把 /vpn/all 各服务面的 config_version 合并成 /vpn/version 的单一版本串
//（分面聚合，2026-07-17）。
//   - 单面：原值返回——与该面 /vpn/all 的 config_version 逐字节一致，单面用户零回归；
//   - 多面：排序后拼接再 hash（顺序无关）。任一面翻转、或面增减（含单面构造失败被 /all 跳过）
//     都翻转合并值——本端点语义是「/vpn/all 会变吗」，必须与 /all 实际内容镜像。
func combineConfigVersions(versions []string) string {
	if len(versions) == 0 {
		return ""
	}
	if len(versions) == 1 {
		return versions[0]
	}
	sorted := append([]string(nil), versions...)
	sort.Strings(sorted)
	data, _ := json.Marshal(sorted)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("v%s", hex.EncodeToString(sum[:8]))
}

// computeConfigVersionWithRegions 阶段2 扩展（契约 §2.4）：哈希覆盖 regions[] 全量——
// 授权集任何【结构性】变化（准入/撤销/租期回收/任一区域换节点/URL/策略变更/state 翻转）
// 都必须翻转 config_version。
//
// ★零回归锚：regions 为空时【委托原 computeConfigVersion】——payload 逐字节同现状，
// 标准面与未升级链路（老 otun 不下发 regions）的 version 不抖动。
// ★防 churn 铁律延续：不纳 is_current / last_used_at（它们随用量/本地切换漂移，纳入则
// 用户一产生流量 version 就变 → 无谓刷新重连）；regions 按 country 排序、每区域
// protocols 按三元组排序、nodes 按 (role,egress_id) 排序——行序漂移不改变 version。
func computeConfigVersionWithRegions(protocols []models.VPNProtocol, smartStrategy json.RawMessage, regions []models.VPNRegion) string {
	if len(regions) == 0 {
		return computeConfigVersion(protocols, smartStrategy)
	}

	type stableProto struct {
		Protocol string `json:"protocol"`
		URL      string `json:"url"`
		Node     string `json:"node"`
	}
	type stableNode struct {
		Role     string `json:"role"`
		EgressID string `json:"egress_id"`
	}
	type stableRegion struct {
		Country       string        `json:"country"`
		State         string        `json:"state"`
		Nodes         []stableNode  `json:"nodes"`
		Protocols     []stableProto `json:"protocols"`
		SmartStrategy interface{}   `json:"smart_strategy"`
	}

	stableProtos := func(ps []models.VPNProtocol) []stableProto {
		sp := make([]stableProto, len(ps))
		for i, p := range ps {
			sp[i] = stableProto{Protocol: p.Protocol, URL: p.URL, Node: p.Node}
		}
		sort.Slice(sp, func(i, j int) bool {
			if sp[i].Node != sp[j].Node {
				return sp[i].Node < sp[j].Node
			}
			if sp[i].Protocol != sp[j].Protocol {
				return sp[i].Protocol < sp[j].Protocol
			}
			return sp[i].URL < sp[j].URL
		})
		return sp
	}
	normalize := func(raw json.RawMessage) interface{} {
		var v interface{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &v); err != nil {
				return string(raw)
			}
		}
		return v
	}

	sr := make([]stableRegion, len(regions))
	for i, r := range regions {
		nodes := make([]stableNode, len(r.Nodes))
		for j, n := range r.Nodes {
			nodes[j] = stableNode{Role: n.Role, EgressID: n.EgressID}
		}
		sort.Slice(nodes, func(a, b int) bool {
			if nodes[a].Role != nodes[b].Role {
				return nodes[a].Role < nodes[b].Role
			}
			return nodes[a].EgressID < nodes[b].EgressID
		})
		sr[i] = stableRegion{
			Country:       r.Country,
			State:         r.State,
			Nodes:         nodes,
			Protocols:     stableProtos(r.Protocols),
			SmartStrategy: normalize(r.SmartStrategy),
		}
	}
	sort.Slice(sr, func(i, j int) bool { return sr[i].Country < sr[j].Country })

	payload := struct {
		Protocols     []stableProto  `json:"protocols"`
		SmartStrategy interface{}    `json:"smart_strategy"`
		Regions       []stableRegion `json:"regions"`
	}{stableProtos(protocols), normalize(smartStrategy), sr}

	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("v%s", hex.EncodeToString(sum[:8]))
}
