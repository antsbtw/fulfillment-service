package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// basic IPv6 方案 A（otun-manager docs/design/BASIC_NODES_IPV6_DELIVERY_DESIGN.md §12.4）：
// fulfillment 必须把 otun-manager 给的 url_v6 / node_id 透传到 /vpn/all，此前 VPNProtocol 只有
// protocol / url / node，其余字段全被丢掉。本文件锁定：
//   1. 标准面 url_v6 / node_id 原样透传，没有的条目不出现这两个键；
//   2. 没有 url_v6 时 config_version 与改造前逐字节一致（名单外用户不抖动）；
//   3. url_v6 加上 / 撤回都翻转 config_version；node_id 不影响 version。

const v6TestVless = "vless://u@13.230.25.114:443?security=reality#OTun-Primary"
const v6TestVlessV6 = "vless://u@[2406:da14:1eb6:b300:e190:ec8b:36d8:d083]:443?security=reality#OTun-Primary"

func TestStandardProtocolsPassThroughURLV6AndNodeID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/subscribe" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"uuid":"u","enabled":true,"expire_at":"2026-12-01T00:00:00Z","protocols":[
			{"protocol":"vless","url":%q,"node":"primary","node_id":"vpn-ap-no-1-20260217-a904","url_v6":%q},
			{"protocol":"shadowsocks","url":"ss://x@13.230.25.114:24238#OTun-Primary","node":"primary","node_id":"vpn-ap-no-1-20260217-a904"}]}`,
			v6TestVless, v6TestVlessV6)
	}))
	defer srv.Close()

	s := &VPNService{
		cfg:        &config.Config{Services: config.ServicesConfig{PublicBaseURL: "https://portal.example.test"}},
		otunClient: client.NewOTunClient(srv.URL, "test-secret"),
	}
	uuid := "u"
	resp, err := s.buildSubscribeResponse(context.Background(), &models.VPNProvision{
		ServiceTier: models.ServiceTierStandard, OtunUUID: &uuid,
	}, "auth-user")
	if err != nil {
		t.Fatalf("buildSubscribeResponse: %v", err)
	}
	if len(resp.Protocols) != 2 {
		t.Fatalf("want 2 protocols, got %+v", resp.Protocols)
	}
	vless, ss := resp.Protocols[0], resp.Protocols[1]
	if vless.URLV6 != v6TestVlessV6 || vless.NodeID != "vpn-ap-no-1-20260217-a904" {
		t.Fatalf("vless url_v6/node_id not passed through: %+v", vless)
	}
	if ss.URLV6 != "" || ss.NodeID != "vpn-ap-no-1-20260217-a904" {
		t.Fatalf("ss must keep node_id and have no url_v6: %+v", ss)
	}

	b, _ := json.Marshal(resp.Protocols)
	if got := strings.Count(string(b), `"url_v6"`); got != 1 {
		t.Fatalf("url_v6 key must appear only on the vless entry, got %d: %s", got, b)
	}
}

// 没有 url_v6 / node_id 的条目：序列化与改造前一样只有四个键。
func TestVPNProtocolOmitsEmptyNewFields(t *testing.T) {
	b, _ := json.Marshal(models.VPNProtocol{Protocol: "vless", URL: "vless://a", Node: "primary"})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 4 {
		t.Fatalf("want exactly protocol/protocol_name/url/node, got %s", b)
	}
}

// legacyConfigVersion 改造前 computeConfigVersion 的原样实现（只投影 protocol/url/node），作对照锚。
func legacyConfigVersion(protocols []models.VPNProtocol, smartStrategy json.RawMessage) string {
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
	var norm interface{}
	if len(smartStrategy) > 0 {
		if err := json.Unmarshal(smartStrategy, &norm); err != nil {
			norm = string(smartStrategy)
		}
	}
	data, _ := json.Marshal(struct {
		Protocols     []stableProto `json:"protocols"`
		SmartStrategy interface{}   `json:"smart_strategy"`
	}{sp, norm})
	sum := sha256.Sum256(data)
	return fmt.Sprintf("v%s", hex.EncodeToString(sum[:8]))
}

func TestConfigVersionURLV6(t *testing.T) {
	strat := json.RawMessage(`{"rules":[{"outbound":"direct"}]}`)
	base := []models.VPNProtocol{
		{Protocol: "vless", URL: v6TestVless, Node: "primary"},
		{Protocol: "shadowsocks", URL: "ss://x", Node: "backup"},
	}
	v0 := computeConfigVersion(base, strat)
	if v0 != legacyConfigVersion(base, strat) {
		t.Fatalf("without url_v6 version must equal pre-change formula: %s vs %s", v0, legacyConfigVersion(base, strat))
	}

	withID := append([]models.VPNProtocol(nil), base...)
	withID[0].NodeID, withID[1].NodeID = "n1", "n2"
	if computeConfigVersion(withID, strat) != v0 {
		t.Fatalf("node_id must not change config_version")
	}

	withV6 := append([]models.VPNProtocol(nil), base...)
	withV6[0].URLV6 = v6TestVlessV6
	v1 := computeConfigVersion(withV6, strat)
	if v1 == v0 {
		t.Fatalf("adding url_v6 must change config_version")
	}
	withV6[0].URLV6 = ""
	if computeConfigVersion(withV6, strat) != v0 {
		t.Fatalf("withdrawing url_v6 must restore the original config_version")
	}
}
