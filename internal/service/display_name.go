package service

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// normalizeDisplayName 与 hosting-service 同一规则(客户端需求 2026-10-31 §B4):去首尾空白,
// 1–40 个 Unicode 字符,拒绝控制字符;空串 = 不设。在这里先校验,免得名字不合法时建机走到 hosting 才失败。
func normalizeDisplayName(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", true
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 40 {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}
