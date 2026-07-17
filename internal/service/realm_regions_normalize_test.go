package service

import (
	"encoding/json"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
)

// ============================================================================
// 2026-07-17 前端真机坐实两偏差的修复：
//   1. select-country / connect-url 响应 regions[] 归一化成契约 §2 形态
//      （RealmResponseWithRegions：protocols[] 展开 + smart_strategy 整包，遮蔽 otun 原始骨架）；
//   2. /vpn/version 分面聚合（combineConfigVersions：单面原值零回归，多面合并 hash）。
// ============================================================================

func sampleOtunRealmResponse() *client.RealmConnectURLResponse {
	ready := true
	return &client.RealmConnectURLResponse{
		OK:         true,
		EgressID:   "eg-de-1",
		ConnectURL: "hysteria2-realm://u@h/realm?realm_id=de-hy2",
		Ready:      &ready,
		Regions: []client.RealmRegion{
			{
				Country: "DE", State: "active", IsCurrent: true,
				Nodes: []client.RealmNode{{
					EgressID: "eg-de-1", Role: "primary", Region: "DE",
					ConnectURLs: []string{
						"hysteria2-realm://u@h/realm?realm_id=de-hy2",
						"trojan-realm://u@h/realm?realm_id=de-trojan",
					},
				}},
				SmartStrategy: json.RawMessage(`{"cn_via_proxy":false}`),
			},
		},
	}
}

// 归一化后 regions[] 必须是契约 §2 形态：protocols[] 由 nodes[].connect_urls 展开、
// smart_strategy 整包透传；otun 原始骨架（无 protocols）被外层遮蔽。
func TestRegionizeRealmResponse_ContractShape(t *testing.T) {
	data, err := json.Marshal(regionizeRealmResponse(sampleOtunRealmResponse()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		OK      bool  `json:"ok"`
		Ready   *bool `json:"ready"`
		Regions []struct {
			Country       string          `json:"country"`
			Protocols     []any           `json:"protocols"`
			SmartStrategy json.RawMessage `json:"smart_strategy"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK || got.Ready == nil || !*got.Ready {
		t.Fatalf("嵌入字段（ok/ready）必须原样透传: %s", data)
	}
	if len(got.Regions) != 1 || got.Regions[0].Country != "DE" {
		t.Fatalf("regions 必须存在且国家正确: %s", data)
	}
	if len(got.Regions[0].Protocols) != 2 {
		t.Fatalf("regions[].protocols 必须由 connect_urls 展开（p0 即前端坐实的偏差）: %s", data)
	}
	if string(got.Regions[0].SmartStrategy) != `{"cn_via_proxy":false}` {
		t.Fatalf("regions[].smart_strategy 必须整包透传: %s", data)
	}
}

// 老 otun 不下发 regions → 归一化后也不得输出 regions key（零回归）。
func TestRegionizeRealmResponse_NoRegionsOmitted(t *testing.T) {
	resp := sampleOtunRealmResponse()
	resp.Regions = nil
	data, err := json.Marshal(regionizeRealmResponse(resp))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["regions"]; ok {
		t.Fatalf("无 regions 时不得输出 regions key: %s", data)
	}
}

// 单面：原值返回（与该面 /vpn/all 的 config_version 逐字节一致，零回归锚）。
func TestCombineConfigVersions_SingleVerbatim(t *testing.T) {
	if got := combineConfigVersions([]string{"vaaaa000011112222"}); got != "vaaaa000011112222" {
		t.Fatalf("单面必须原值返回: got %q", got)
	}
}

// 多面：任一面翻转必翻转合并值；顺序无关；合并值不等于任一单面值。
func TestCombineConfigVersions_MultiFace(t *testing.T) {
	std, res := "vaaaa000011112222", "vbbbb000011112222"
	v0 := combineConfigVersions([]string{std, res})
	if v0 == std || v0 == res {
		t.Fatalf("多面合并值不得等于任一单面值: %q", v0)
	}
	if got := combineConfigVersions([]string{res, std}); got != v0 {
		t.Fatalf("合并必须顺序无关: %q vs %q", got, v0)
	}
	resChanged := "vcccc000011112222"
	if got := combineConfigVersions([]string{std, resChanged}); got == v0 {
		t.Fatalf("住宅面翻转后合并值必须翻转（前端坐实的问题4）")
	}
	// 面增减（如住宅面构造失败被 /all 跳过）也必须翻转。
	if got := combineConfigVersions([]string{std}); got == v0 {
		t.Fatalf("面数量变化必须翻转合并值")
	}
}
