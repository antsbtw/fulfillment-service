package service

import "testing"

func TestIsHostedNode(t *testing.T) {
	cases := map[string]bool{
		"obox-0-cacbc7": true,  // 老式托管
		"obox-0-ca68ad": true,  // 新式托管 hosted_v2
		"byo-c8a32e55":  false, // 用户自建(token 接入)
		"":              false,
	}
	for id, want := range cases {
		if got := isHostedNode(id); got != want {
			t.Errorf("isHostedNode(%q) = %v, want %v", id, got, want)
		}
	}
}
