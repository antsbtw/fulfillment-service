package http

import "testing"

// 验收 F3：换绑回收的分区判定按产品面；campaign 拒绝，不得折成 basic。
func TestDeprovisionPartitionFor(t *testing.T) {
	tr := true
	cases := []struct {
		plan     string
		explicit *bool
		wantNil  bool
		wantRes  bool
		reject   bool
	}{
		{"", nil, true, false, false},
		{"basic", nil, false, false, false},
		{"premium", nil, false, false, false},
		{"residential", nil, false, true, false},
		{"campaign", nil, false, false, true},
		{"campaign", &tr, false, false, true}, // 显式 is_residential 也不能把 campaign 请求放行
		{"", &tr, false, true, false},
	}
	for _, c := range cases {
		got, reject := deprovisionPartitionFor(c.plan, c.explicit)
		if reject != c.reject {
			t.Fatalf("plan=%q reject=%v want %v", c.plan, reject, c.reject)
		}
		if c.reject {
			continue
		}
		if c.wantNil {
			if got != nil {
				t.Fatalf("plan=%q want nil partition, got %v", c.plan, *got)
			}
			continue
		}
		if got == nil || *got != c.wantRes {
			t.Fatalf("plan=%q want isResidential=%v got %v", c.plan, c.wantRes, got)
		}
	}
}
