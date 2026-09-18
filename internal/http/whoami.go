package http

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Whoami 返回"这次请求是从哪个公网出口打过来的"（公开，无鉴权）。
//
// 用途：App 连接后的可达探测判据。以前判"能不能拿到 204"，而探测目标
// （gstatic 等）在大陆裸网可达、且命中 geosite-cn 走直连，任何泄漏路径都会
// 假成功（2026-09-17 实证）。改判"这次往返是不是真从所选出口出去的"：
// App 经隧道请求本端点，country == 所选出口国才算通；走了直连会拿到 CN。
//
// 约定（前端 2026-09-17 提出）：
//   - 无鉴权：连接建立阶段可能没有有效 token，带鉴权会把"隧道不通"与
//     "token 过期"混成同一个失败；
//   - 响应极小、无重定向、Cache-Control: no-store；
//   - 域名 portal.situstechnologies.com 已验证不在 geosite-cn。
//
// IP/国家取 Cloudflare 注入的 CF-Connecting-IP / CF-IPCountry（本服务只经
// CF → nginx 对外）。country 取不到（头缺失、XX=未知、T1=Tor）时为空串，
// 由 App 按"无法判定"处理，不要当成不匹配。
func (h *Handler) Whoami(c *gin.Context) {
	ip := strings.TrimSpace(c.GetHeader("CF-Connecting-IP"))
	if ip == "" {
		ip = c.ClientIP()
	}
	country := strings.ToUpper(strings.TrimSpace(c.GetHeader("CF-IPCountry")))
	if len(country) != 2 || country == "XX" || country == "T1" {
		country = ""
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"ip": ip, "country": country})
}
