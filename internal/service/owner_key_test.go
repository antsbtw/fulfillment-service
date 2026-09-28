package service

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func edKey(t *testing.T) string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
}

func TestNormalizeOwnerKey(t *testing.T) {
	k := edKey(t)
	got, err := NormalizeOwnerKey(k + " iPhone 15 Pro")
	if err != nil || got != k+" iPhone 15 Pro" {
		t.Fatalf("plain: %q %v", got, err)
	}
	// 注入字符被清掉,整行只剩安全字符
	got, err = NormalizeOwnerKey(k + " a';rm -rf /;$(id)`x`")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, k+" ") || strings.ContainsAny(strings.TrimPrefix(got, k), "';$()`/") {
		t.Fatalf("unsanitized: %q", got)
	}
	// 备注截断
	got, _ = NormalizeOwnerKey(k + " " + strings.Repeat("a", 200))
	if len(got) > len(k)+1+ownerKeyCommentMax {
		t.Fatalf("comment not truncated: %d", len(got))
	}
}

func TestNormalizeOwnerKey_Rejects(t *testing.T) {
	k := edKey(t)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaPub, _ := ssh.NewPublicKey(&rk.PublicKey)
	for name, in := range map[string]string{
		"empty":    "",
		"two keys": k + "\n" + k,
		"options":  `command="sh" ` + k,
		"rsa":      strings.TrimSpace(string(ssh.MarshalAuthorizedKey(rsaPub))),
		"garbage":  "ssh-ed25519 notbase64!!",
		"private":  "-----BEGIN OPENSSH PRIVATE KEY-----",
		"nul":      k + "\x00",
	} {
		if _, err := NormalizeOwnerKey(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
