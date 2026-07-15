package service

import (
	"encoding/json"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// ============================================================================
// 阶段2 提交F：config_version 覆盖 regions[]（契约 §2.4）+ buildVPNRegions 映射。
// 核心零回归锚：无 regions 时 WithRegions ≡ 原 computeConfigVersion（委托，逐字节同 payload）
// ——标准面与未升级链路（老 otun）的 version 绝不因本次改动抖动。
// ============================================================================

func sampleRegions() []models.VPNRegion {
	return []models.VPNRegion{
		{
			Country: "JP", State: "active", IsCurrent: true,
			Nodes: []models.RealmNodeSummary{{Role: "primary", EgressID: "eg-jp-1"}, {Role: "backup", EgressID: "eg-jp-2"}},
			Protocols: []models.VPNProtocol{
				{Protocol: "hysteria2-realm", URL: "hysteria2-realm://u@h/realm?realm_id=jp-hy2", Node: "primary"},
				{Protocol: "trojan-realm", URL: "trojan-realm://u@h/realm?realm_id=jp-trojan", Node: "backup"},
			},
			SmartStrategy: json.RawMessage(`{"cn_via_proxy":false}`),
		},
		{
			Country: "SG", State: "active", IsCurrent: false,
			Nodes: []models.RealmNodeSummary{{Role: "primary", EgressID: "eg-sg-1"}},
			Protocols: []models.VPNProtocol{
				{Protocol: "hysteria2-realm", URL: "hysteria2-realm://u@h/realm?realm_id=sg-hy2", Node: "primary"},
			},
			SmartStrategy: json.RawMessage(`{"cn_via_proxy":false}`),
		},
	}
}

// ★零回归锚：regions 为空（nil 与空切片）→ 与原函数逐字节同值。
func TestConfigVersionWithRegions_NilMatchesLegacy(t *testing.T) {
	strat := json.RawMessage(`{"cn_via_proxy":true}`)
	legacy := computeConfigVersion(sampleProtos(), strat)
	if got := computeConfigVersionWithRegions(sampleProtos(), strat, nil); got != legacy {
		t.Fatalf("nil regions must equal legacy hash: got %q want %q", got, legacy)
	}
	if got := computeConfigVersionWithRegions(sampleProtos(), strat, []models.VPNRegion{}); got != legacy {
		t.Fatalf("empty regions must equal legacy hash: got %q want %q", got, legacy)
	}
}

// regions 行序/区域内 protocols 行序打乱 → version 不变（防 churn 排序）。
func TestConfigVersionWithRegions_OrderStable(t *testing.T) {
	strat := json.RawMessage(`{"cn_via_proxy":true}`)
	v0 := computeConfigVersionWithRegions(sampleProtos(), strat, sampleRegions())

	shuffled := sampleRegions()
	shuffled[0], shuffled[1] = shuffled[1], shuffled[0]
	shuffled[1].Protocols[0], shuffled[1].Protocols[1] = shuffled[1].Protocols[1], shuffled[1].Protocols[0]
	shuffled[1].Nodes[0], shuffled[1].Nodes[1] = shuffled[1].Nodes[1], shuffled[1].Nodes[0]
	if got := computeConfigVersionWithRegions(sampleProtos(), strat, shuffled); got != v0 {
		t.Fatalf("version drifted on region/protocol/node row order: %q vs %q", got, v0)
	}
}

// 授权集结构性变化必翻转：准入（加区域）/撤销（减区域）/换节点（URL 变）/state 翻转/该区策略变。
// is_current 翻转（用量漂移）绝不翻转 version（防 churn 铁律）。
func TestConfigVersionWithRegions_ChangeDetection(t *testing.T) {
	strat := json.RawMessage(`{"cn_via_proxy":true}`)
	base := computeConfigVersionWithRegions(sampleProtos(), strat, sampleRegions())

	// 准入第三区域。
	added := append(sampleRegions(), models.VPNRegion{Country: "US", State: "active",
		Protocols: []models.VPNProtocol{{Protocol: "hysteria2-realm", URL: "hysteria2-realm://u@h/r?realm_id=us", Node: "primary"}}})
	if computeConfigVersionWithRegions(sampleProtos(), strat, added) == base {
		t.Fatalf("admitting a region must flip version")
	}
	// 撤销一个区域。
	if computeConfigVersionWithRegions(sampleProtos(), strat, sampleRegions()[:1]) == base {
		t.Fatalf("evicting a region must flip version")
	}
	// 区域内换节点（URL 变）。
	changed := sampleRegions()
	changed[1].Protocols[0].URL = "hysteria2-realm://u@OTHER/realm?realm_id=sg2-hy2"
	if computeConfigVersionWithRegions(sampleProtos(), strat, changed) == base {
		t.Fatalf("node/url change inside a region must flip version")
	}
	// state 翻转（switching→active 确认）。
	st := sampleRegions()
	st[1].State = "switching"
	if computeConfigVersionWithRegions(sampleProtos(), strat, st) == base {
		t.Fatalf("region state change must flip version")
	}
	// 区域策略变。
	sc := sampleRegions()
	sc[0].SmartStrategy = json.RawMessage(`{"cn_via_proxy":false,"extra_direct_domains":["x.com"]}`)
	if computeConfigVersionWithRegions(sampleProtos(), strat, sc) == base {
		t.Fatalf("region strategy change must flip version")
	}

	// ★is_current 翻转（本地切换/流量漂移）→ version 不变。
	cur := sampleRegions()
	cur[0].IsCurrent, cur[1].IsCurrent = false, true
	if computeConfigVersionWithRegions(sampleProtos(), strat, cur) != base {
		t.Fatalf("is_current flip is usage-driven and must NOT flip version (anti-churn)")
	}
}

// buildVPNRegions 映射：connect_urls 展开成 protocols（node=role）、摘要与策略整包透传、空→nil。
func TestBuildVPNRegions(t *testing.T) {
	if got := buildVPNRegions(nil); got != nil {
		t.Fatalf("nil regions must map to nil (omitempty 零回归): %v", got)
	}

	in := []client.RealmRegion{{
		Country: "JP", State: "active", IsCurrent: true,
		Nodes: []client.RealmNode{
			{EgressID: "eg-1", Role: "primary", Region: "tokyo",
				ConnectURL:  "hysteria2-realm://u@h/realm?realm_id=jp-hy2",
				ConnectURLs: []string{"hysteria2-realm://u@h/realm?realm_id=jp-hy2", "trojan-realm://u@h/realm?realm_id=jp-trojan", "vless://not-realm"}},
			{EgressID: "eg-2", Role: "backup",
				ConnectURLs: nil, ConnectURL: "hysteria2-realm://u@h2/realm?realm_id=jp2-hy2"}, // 单 hy2 兜底
		},
		SmartStrategy: json.RawMessage(`{"cn_via_proxy":false}`),
		LastUsedAt:    "2026-07-15T00:00:00Z",
	}}
	out := buildVPNRegions(in)
	if len(out) != 1 {
		t.Fatalf("want 1 region, got %d", len(out))
	}
	r := out[0]
	if r.Country != "JP" || r.State != "active" || !r.IsCurrent {
		t.Fatalf("region meta wrong: %+v", r)
	}
	if len(r.Nodes) != 2 || r.Nodes[0].EgressID != "eg-1" || r.Nodes[0].Role != "primary" || r.Nodes[0].Region != "tokyo" {
		t.Fatalf("node summaries wrong: %+v", r.Nodes)
	}
	// 2（primary 六协议里合法的两条）+ 1（backup 单 hy2 兜底）——非 realm scheme 被滤掉。
	if len(r.Protocols) != 3 {
		t.Fatalf("want 3 protocols (invalid scheme filtered), got %d: %+v", len(r.Protocols), r.Protocols)
	}
	for _, p := range r.Protocols {
		if p.Node != "primary" && p.Node != "backup" {
			t.Fatalf("protocol node must be role: %+v", p)
		}
	}
	if string(r.SmartStrategy) != `{"cn_via_proxy":false}` {
		t.Fatalf("strategy must pass through untouched: %s", r.SmartStrategy)
	}
}
