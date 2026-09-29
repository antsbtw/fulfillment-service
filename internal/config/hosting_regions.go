package config

// 托管区域运营名单默认值(可用 HOSTING_REGION_DENY / HOSTING_REGION_ORDER 覆盖,逗号分隔,"-" 清空)。
// 区域目录本身来自 hosting 的 Lightsail GetRegions;不在 deny 里的区域默认开放。

// DefaultHostingRegionDeny 不对外提供的区域。改动时在这里写明原因,一行一个。
var DefaultHostingRegionDeny = []string{
	"ap-south-1",     // 孟买:同价套餐流量额度只有其他区域一半(Basic 承诺 1TB 会产生超额费),国内回程普遍绕行
	"ap-southeast-2", // 悉尼:同上,流量额度减半;国内延迟明显高于日韩新
}

// DefaultHostingRegionOrder 显示顺序:亚太在前,再美洲、欧洲。未列出的新区域按 code 排在最后。
var DefaultHostingRegionOrder = []string{
	"ap-northeast-1", "ap-northeast-2", "ap-southeast-1", "ap-southeast-3", "ap-southeast-2", "ap-south-1",
	"us-west-2", "us-east-1", "us-east-2", "ca-central-1",
	"eu-west-2", "eu-central-1", "eu-west-1", "eu-west-3", "eu-north-1",
}
