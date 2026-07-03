package service

import (
	"encoding/json"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// Batch 3（§8.3）：config_version 稳定性单测，遵防 churn 铁律（§5.2）。
// 钉死：①protocols 行序漂移 → version 不变（顺序维度）；②smart_strategy JSON 键序/空白差异 → 不变；
//   ③真变化（URL/node/strategy 值）→ version 变。

func sampleProtos() []models.VPNProtocol {
	return []models.VPNProtocol{
		{Protocol: "hysteria2-realm", URL: "hysteria2-realm://u@h/realm?realm_id=a-hy2", Node: "primary"},
		{Protocol: "reality-realm", URL: "reality-realm://u@h/realm?realm_id=a-reality", Node: "primary"},
		{Protocol: "ss-realm", URL: "ss-realm://u@h/realm?realm_id=a-ss", Node: "primary"},
	}
}

// ①protocols 行序打乱（同一份集合，逆序）→ config_version 不变。
func TestConfigVersionStableOnProtocolRowOrder(t *testing.T) {
	strat := json.RawMessage(`{"cn_via_proxy":true,"dns_proxy":"223.5.5.5"}`)
	v0 := computeConfigVersion(sampleProtos(), strat)

	shuffled := []models.VPNProtocol{sampleProtos()[2], sampleProtos()[0], sampleProtos()[1]}
	if got := computeConfigVersion(shuffled, strat); got != v0 {
		t.Fatalf("config_version drifted on protocol row order: got %q want %q", got, v0)
	}
}

// ②smart_strategy 键序/空白不同但语义相同 → config_version 不变（归一化生效）。
func TestConfigVersionStableOnStrategyKeyOrder(t *testing.T) {
	protos := sampleProtos()
	a := json.RawMessage(`{"cn_via_proxy":true,"dns_proxy":"223.5.5.5","extra_direct_domains":["cctv.com"]}`)
	b := json.RawMessage(`{  "extra_direct_domains":["cctv.com"] , "dns_proxy":"223.5.5.5",  "cn_via_proxy":true }`)
	if computeConfigVersion(protos, a) != computeConfigVersion(protos, b) {
		t.Fatalf("config_version should be stable across smart_strategy key order/whitespace")
	}
}

// ③真变化必须翻转 version（URL 变 = 出口/凭证变；strategy 值变；node 变）。
func TestConfigVersionChangesOnRealChange(t *testing.T) {
	strat := json.RawMessage(`{"cn_via_proxy":true}`)
	base := computeConfigVersion(sampleProtos(), strat)

	// URL 变（换出口）。
	p2 := sampleProtos()
	p2[0].URL = "hysteria2-realm://u@OTHERHOST/realm?realm_id=b-hy2"
	if computeConfigVersion(p2, strat) == base {
		t.Fatalf("config_version must change when a protocol URL changes")
	}

	// smart_strategy 值变（补漏加域名）。
	if computeConfigVersion(sampleProtos(), json.RawMessage(`{"cn_via_proxy":true,"extra_direct_domains":["tv.cctv.com"]}`)) == base {
		t.Fatalf("config_version must change when smart_strategy changes")
	}

	// node 变（primary↔backup）。
	p3 := sampleProtos()
	p3[0].Node = "backup"
	if computeConfigVersion(p3, strat) == base {
		t.Fatalf("config_version must change when node changes")
	}
}

// 标准分支（无 smart_strategy）也产出确定非空 version，且行序稳定。
func TestConfigVersionStandardNoStrategy(t *testing.T) {
	protos := []models.VPNProtocol{
		{Protocol: "vless", URL: "vless://a", Node: "primary"},
		{Protocol: "shadowsocks", URL: "ss://b", Node: "backup"},
	}
	v0 := computeConfigVersion(protos, nil)
	if v0 == "" || v0[0] != 'v' {
		t.Fatalf("standard config_version should be non-empty v-prefixed: %q", v0)
	}
	rev := []models.VPNProtocol{protos[1], protos[0]}
	if computeConfigVersion(rev, nil) != v0 {
		t.Fatalf("standard config_version should be row-order stable")
	}
}
