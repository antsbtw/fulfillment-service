package service

// 与 hosting-service internal/service/owner_key.go 同一实现(两处须同改)。fulfillment 用它同步回 400,hosting 再校验一次。

import (
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ErrInvalidOwnerKey owner_key 不是恰好一把可用的 ssh-ed25519 公钥。
var ErrInvalidOwnerKey = errors.New("owner_key must be exactly one ssh-ed25519 public key")

const ownerKeyCommentMax = 64

// NormalizeOwnerKey 校验并重写 App 提交的 owner key(回执 R-12 ①)。
//
// 它会被写进 Lightsail 启动脚本,是命令注入面:只接受恰好一把 ssh-ed25519,
// 按"类型 + base64 + 清洗过的备注"重新拼出,绝不原样使用输入。
// 备注只保留字母、数字、空格与 -_.@,最长 64 字符。
func NormalizeOwnerKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", ErrInvalidOwnerKey
	}
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(raw))
	if err != nil || len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
		return "", ErrInvalidOwnerKey
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return "", ErrInvalidOwnerKey
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) // "ssh-ed25519 <b64>"
	if c := sanitizeKeyComment(comment); c != "" {
		line += " " + c
	}
	return line, nil
}

func sanitizeKeyComment(c string) string {
	var b strings.Builder
	for _, r := range c {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_', r == '.', r == '@':
			b.WriteRune(r)
		}
		if b.Len() >= ownerKeyCommentMax {
			break
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
