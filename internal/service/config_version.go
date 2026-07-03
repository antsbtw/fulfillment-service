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
