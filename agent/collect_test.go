package main

// finalizeTemps / 温度聚合的单元测试。
// 这些函数在 collect.go 里，与平台无关，任何系统上都能跑。

import (
	"reflect"
	"testing"
)

func TestFinalizeTempsFiltersBadReadings(t *testing.T) {
	in := []TempStat{
		{Name: "ok", Kind: TempKindCPU, Source: "a", TempC: 55},
		{Name: "zero", Kind: TempKindCPU, Source: "b", TempC: 0},
		{Name: "negative", Kind: TempKindCPU, Source: "c", TempC: -10},
		{Name: "register-garbage", Kind: TempKindBoard, Source: "d", TempC: 255},
		{Name: "above-limit", Kind: TempKindBoard, Source: "e", TempC: 151},
	}
	got := finalizeTemps(in, &Config{})
	if len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("异常读数必须被丢掉，实际保留: %+v", got)
	}
}

func TestFinalizeTempsDedup(t *testing.T) {
	in := []TempStat{
		{Name: "a", Kind: TempKindCPU, Source: "hwmon:coretemp/temp1", TempC: 55},
		{Name: "a-again", Kind: TempKindCPU, Source: "hwmon:coretemp/temp1", TempC: 56},
		// 没有 Source 时按 Kind+Name 去重
		{Name: "n1", Kind: TempKindBoard, TempC: 40},
		{Name: "n1", Kind: TempKindBoard, TempC: 41},
	}
	got := finalizeTemps(in, &Config{})
	if len(got) != 2 {
		t.Fatalf("期望去重后 2 个，实际 %d: %+v", len(got), got)
	}
	if got[0].TempC != 55 {
		t.Errorf("去重应保留先出现的那个，实际 %v", got[0].TempC)
	}
}

func TestFinalizeTempsKindDefaultsAndRank(t *testing.T) {
	in := []TempStat{
		{Name: "nic", Kind: TempKindNIC, Source: "n", TempC: 50},
		{Name: "other", Kind: TempKindOther, Source: "o", TempC: 50},
		{Name: "board", Kind: TempKindBoard, Source: "b", TempC: 50},
		{Name: "disk", Kind: TempKindDisk, Source: "d", TempC: 50},
		{Name: "gpu", Kind: TempKindGPU, Source: "g", TempC: 50},
		{Name: "cpu", Kind: TempKindCPU, Source: "c", TempC: 50},
		{Name: "no-kind", Source: "x", TempC: 50}, // Kind 为空 → other
	}
	got := finalizeTemps(in, &Config{})
	// 顺序由 tempKindRank 决定：CPU > GPU > 硬盘 > 主板 > 网卡 > 其他
	wantOrder := []string{TempKindCPU, TempKindGPU, TempKindDisk, TempKindBoard, TempKindNIC, TempKindOther, TempKindOther}
	if len(got) != len(wantOrder) {
		t.Fatalf("期望 %d 项，实际 %d: %+v", len(wantOrder), len(got), got)
	}
	for i, w := range wantOrder {
		if got[i].Kind != w {
			t.Errorf("第 %d 项类别 = %q，期望 %q", i, got[i].Kind, w)
		}
	}
	if n := lastKindOtherCount(got); n != 2 {
		t.Errorf("other 类应聚集在末尾且共 2 条，实际 %d", n)
	}
}

func lastKindOtherCount(list []TempStat) int {
	n := 0
	for _, s := range list {
		if s.Kind == TempKindOther {
			n++
		}
	}
	return n
}

func TestFinalizeTempsSortStableWithinKind(t *testing.T) {
	in := []TempStat{
		{Name: "CPU · b", Kind: TempKindCPU, Source: "1", TempC: 40},
		{Name: "CPU · a", Kind: TempKindCPU, Source: "2", TempC: 70},
	}
	got := finalizeTemps(in, &Config{})
	if got[0].Name != "CPU · a" || got[1].Name != "CPU · b" {
		t.Fatalf("同类别内应按名称排序，保证多次采样顺序一致：%+v", got)
	}
}

func TestFinalizeTempsLimit(t *testing.T) {
	var in []TempStat
	for i := 0; i < 10; i++ {
		in = append(in, TempStat{
			Name: string(rune('a' + i)), Kind: TempKindCPU,
			Source: string(rune('a' + i)), TempC: 50,
		})
	}
	got := finalizeTemps(in, &Config{Collect: CollectConfig{TempLimit: 3}})
	if len(got) != 3 {
		t.Fatalf("TempLimit=3 应截断到 3 条，实际 %d", len(got))
	}
	// limit 为 0 表示不限制
	if n := len(finalizeTemps(in, &Config{})); n != 10 {
		t.Errorf("未设置 limit 时不该截断，实际 %d", n)
	}
}

func TestFinalizeTempsEmpty(t *testing.T) {
	if got := finalizeTemps(nil, &Config{}); got != nil {
		t.Errorf("空输入应返回 nil，实际 %+v", got)
	}
	if got := finalizeTemps([]TempStat{{Name: "x", TempC: 0}}, &Config{}); got != nil {
		t.Errorf("全被过滤后应返回 nil，实际 %+v", got)
	}
}

func TestExcludedTemp(t *testing.T) {
	pats := []string{"Composite", "hwmon:nvme", "", "  "}
	if !excludedTemp(TempStat{Name: "硬盘 · Composite"}, pats) {
		t.Error("应按 Name 子串排除")
	}
	if !excludedTemp(TempStat{Source: "hwmon:nvme/temp1"}, pats) {
		t.Error("应按 Source 子串排除")
	}
	if excludedTemp(TempStat{Name: "CPU · Package", Source: "hwmon:coretemp/temp1"}, pats) {
		t.Error("不该误伤无关传感器")
	}
	if excludedTemp(TempStat{Name: "任意", Source: "任意"}, []string{"", "   "}) {
		t.Error("空串规则必须被忽略，否则会排除掉所有传感器")
	}
}

func TestMaxTempOfKind(t *testing.T) {
	temps := []TempStat{
		{Kind: TempKindCPU, TempC: 55},
		{Kind: TempKindCPU, TempC: 72},
		{Kind: TempKindDisk, TempC: 41},
		{Kind: TempKindGPU, TempC: 88},
	}
	if got := maxTempOfKind(temps, ""); got != 88 {
		t.Errorf("不指定类别应取全局最高温 88，实际 %v", got)
	}
	if got := maxTempOfKind(temps, TempKindCPU); got != 72 {
		t.Errorf("CPU 最高温应为 72，实际 %v", got)
	}
	if got := maxTempOfKind(temps, TempKindNIC); got != 0 {
		t.Errorf("不存在的类别应返回 0，实际 %v", got)
	}
	if got := maxTempOfKind(nil, ""); got != 0 {
		t.Errorf("空列表应返回 0，实际 %v", got)
	}
}

func TestShortGPUName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"NVIDIA GeForce RTX 4090", "RTX 4090"},
		{"NVIDIA A100-SXM4-80GB", "A100-SXM4-80GB"},
		{"AMD Radeon RX 7900 XTX", "RX 7900 XTX"},
		{"RTX 2060 with Max-Q Design", "RTX 2060 with Max-Q Design"},
	}
	for _, c := range cases {
		if got := shortGPUName(c.in); got != c.want {
			t.Errorf("shortGPUName(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 温度配置的默认值与边界钳制。
func TestTempConfigDefaultsAndClamp(t *testing.T) {
	c := defaultConfig()
	if !c.Collect.Temp {
		t.Error("温度采集应默认开启")
	}
	if c.Collect.TempLimit != 32 {
		t.Errorf("TempLimit 默认应为 32，实际 %d", c.Collect.TempLimit)
	}

	// validate() 会连带校验 node_id / master_url / token，先补一份最小可用配置
	fill := func(c *Config) {
		c.NodeID = "test-node"
		c.MasterURL = "https://master.example.com/api/v1/report"
		c.Token = "0123456789abcdef0123"
	}

	fill(c)
	c.Collect.TempLimit = 9999
	c.Collect.TempExclude = []string{"  Composite  ", " ", ""}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.Collect.TempLimit != 128 {
		t.Errorf("TempLimit 应被钳到 128，实际 %d", c.Collect.TempLimit)
	}
	if !reflect.DeepEqual(c.Collect.TempExclude, []string{"Composite"}) {
		t.Errorf("TempExclude 应去掉空白项并 trim，实际 %v", c.Collect.TempExclude)
	}

	// 0 / 负数回落默认值
	c2 := defaultConfig()
	fill(c2)
	c2.Collect.TempLimit = -1
	_ = c2.validate()
	if c2.Collect.TempLimit != 32 {
		t.Errorf("TempLimit=-1 应回落 32，实际 %d", c2.Collect.TempLimit)
	}
}
