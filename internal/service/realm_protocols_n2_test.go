package service

import (
	"encoding/json"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 2c：buildRealmProtocolsN2 展 primary6+backup6(node 区分主备,region 不挂 protocol);
// buildRealmNodeSummaries 出口级 region 摘要;逐级 fail-soft。

func sixURLs(egSuffix string) []string {
	return []string{
		"hysteria2-realm://u@h/realm?realm_id=" + egSuffix + "-hy2",
		"reality-realm://u@h/realm?realm_id=" + egSuffix + "-reality",
		"tuic-realm://u@h/realm?realm_id=" + egSuffix + "-tuic",
		"trojan-realm://u@h/realm?realm_id=" + egSuffix + "-trojan",
		"ss-realm://u@h/realm?realm_id=" + egSuffix + "-ss",
		"vmess-realm://u@h/realm?realm_id=" + egSuffix + "-vmess",
	}
}

// N=2 两出口各六协议 → 12 条，node=role。★region 不再挂 protocol（改由顶层 nodes[] 摘要表达）。
func TestBuildRealmProtocolsN2_Expands12(t *testing.T) {
	nodes := []client.RealmNode{
		{EgressID: "eg-a", Role: "primary", Region: "上海移动", ConnectURLs: sixURLs("a")},
		{EgressID: "eg-b", Role: "backup", Region: "北京联通", ConnectURLs: sixURLs("b")},
	}
	got := buildRealmProtocolsN2(nodes, nil, "")
	if len(got) != 12 {
		t.Fatalf("expected 12 protocols (2x6), got %d", len(got))
	}
	var primaryN, backupN int
	for _, p := range got {
		switch p.Node {
		case "primary":
			primaryN++
		case "backup":
			backupN++
		default:
			t.Fatalf("unexpected node role: %q", p.Node)
		}
		if p.Protocol == "" || p.URL == "" {
			t.Fatalf("empty protocol/url: %+v", p)
		}
	}
	if primaryN != 6 || backupN != 6 {
		t.Fatalf("expected 6 primary + 6 backup, got %d/%d", primaryN, backupN)
	}
}

// ★2c：buildRealmNodeSummaries 收敛出口摘要——region 挂出口级（每出口 1 次）。
func TestBuildRealmNodeSummaries(t *testing.T) {
	// N=2：2 出口 → 2 摘要，region 各一次。
	nodes := []client.RealmNode{
		{EgressID: "eg-a", Role: "primary", Region: "上海移动", ConnectURLs: sixURLs("a")},
		{EgressID: "eg-b", Role: "backup", Region: "北京联通", ConnectURLs: sixURLs("b")},
	}
	sum := buildRealmNodeSummaries(nodes)
	if len(sum) != 2 {
		t.Fatalf("N=2 should give 2 node summaries, got %d", len(sum))
	}
	if sum[0].Role != "primary" || sum[0].Region != "上海移动" || sum[0].EgressID != "eg-a" {
		t.Fatalf("primary summary wrong: %+v", sum[0])
	}
	if sum[1].Role != "backup" || sum[1].Region != "北京联通" || sum[1].EgressID != "eg-b" {
		t.Fatalf("backup summary wrong: %+v", sum[1])
	}

	// N=1：单出口 → 1 摘要。
	one := buildRealmNodeSummaries(nodes[:1])
	if len(one) != 1 || one[0].Role != "primary" {
		t.Fatalf("N=1 should give 1 summary: %+v", one)
	}

	// nodes 空（老 otun / 单出口 fallback）→ nil（前端不读也不坏，零回归）。
	if buildRealmNodeSummaries(nil) != nil {
		t.Fatalf("empty nodes should give nil summary")
	}

	// 无 role → 默认 primary。
	nr := buildRealmNodeSummaries([]client.RealmNode{{EgressID: "eg-x", Region: "广东电信"}})
	if nr[0].Role != "primary" {
		t.Fatalf("empty role → primary: %+v", nr[0])
	}
}

// N=1（池降级，只 primary）→ 6 条全 primary。
func TestBuildRealmProtocolsN2_DegradedN1(t *testing.T) {
	nodes := []client.RealmNode{{EgressID: "eg-a", Role: "primary", Region: "上海移动", ConnectURLs: sixURLs("a")}}
	got := buildRealmProtocolsN2(nodes, nil, "")
	if len(got) != 6 {
		t.Fatalf("N=1 should give 6, got %d", len(got))
	}
	for _, p := range got {
		if p.Node != "primary" {
			t.Fatalf("all should be primary: %+v", p)
		}
	}
}

// nodes 空 → fallback 到单出口 connect_urls（全 primary，2b-1 前老形态，向后兼容）。
func TestBuildRealmProtocolsN2_FallbackToConnectURLs(t *testing.T) {
	got := buildRealmProtocolsN2(nil, sixURLs("a"), "hysteria2-realm://u@h/realm?realm_id=a-hy2")
	if len(got) != 6 {
		t.Fatalf("fallback should expand connect_urls to 6, got %d", len(got))
	}
	for _, p := range got {
		if p.Node != "primary" {
			t.Fatalf("fallback protocols should be primary: %+v", p)
		}
	}
}

// nodes 空 + connect_urls 空 → 单条 hy2 兜底（旧 manager 零回归）。
func TestBuildRealmProtocolsN2_FallbackSingleHY2(t *testing.T) {
	got := buildRealmProtocolsN2(nil, nil, "hysteria2-realm://u@h/realm?realm_id=a-hy2")
	if len(got) != 1 || got[0].Protocol != "hysteria2-realm" || got[0].Node != "primary" {
		t.Fatalf("should fall back to single hy2: %+v", got)
	}
}

// node 无 role → 默认 primary；node 只有单 ConnectURL(无 ConnectURLs) → 用它。
func TestBuildRealmProtocolsN2_NodeEdgeCases(t *testing.T) {
	nodes := []client.RealmNode{
		{EgressID: "eg-a", Role: "", ConnectURL: "hysteria2-realm://u@h/realm?realm_id=a-hy2"},
	}
	got := buildRealmProtocolsN2(nodes, nil, "")
	if len(got) != 1 || got[0].Node != "primary" {
		t.Fatalf("empty role → primary, single ConnectURL used: %+v", got)
	}
}

// ★2c：protocol【绝不】含 region key（出口级归属改由 nodes[] 摘要表达）。
func TestVPNProtocolNeverEmitsRegion(t *testing.T) {
	p := models.VPNProtocol{Protocol: "hysteria2-realm", URL: "hy2://x", Node: "backup"}
	b, _ := p.MarshalJSON()
	if contains2c(string(b), "region") {
		t.Fatalf("protocol must NOT emit region key (moved to nodes[] summary): %s", b)
	}
	// 仍输出 protocol/protocol_name/url/node 四键（前端双 key 契约不破）。
	for _, k := range []string{"protocol", "protocol_name", "url", "node"} {
		if !contains2c(string(b), k) {
			t.Fatalf("protocol missing key %q: %s", k, b)
		}
	}
}

// RealmNodeSummary：region 空 → omitempty 不输出；有值 → 输出。
func TestRealmNodeSummaryRegionOmitempty(t *testing.T) {
	noRegion := models.RealmNodeSummary{Role: "primary", EgressID: "eg-a"}
	b, _ := json.Marshal(noRegion)
	if contains2c(string(b), "region") {
		t.Fatalf("empty region must be omitted: %s", b)
	}
	withRegion := models.RealmNodeSummary{Role: "backup", Region: "北京联通", EgressID: "eg-b"}
	rb, _ := json.Marshal(withRegion)
	if !contains2c(string(rb), "北京联通") {
		t.Fatalf("node summary must emit region: %s", rb)
	}
}

func contains2c(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
