package main

import "testing"

// 温度指标解析与聚合的单元测试（master 侧）。
// 看板能画温度的前提是这几个函数把 "temp:cpu" 这类指标名正确解开，
// 而它们很容易在加新指标时被改坏。

func TestTempMetricKind(t *testing.T) {
	cases := []struct {
		in   string
		kind string
		ok   bool
	}{
		{"temp", "", true},
		{"temp:cpu", "cpu", true},
		{"temp:gpu", "gpu", true},
		{"temp:disk", "disk", true},
		{"temp:board", "board", true},
		{"temp:nic", "nic", true},
		{"temp:other", "other", true},
		// 未知类别要放行：否则 agent 新增 kind 时历史查询会静默变空图
		{"temp:bogus", "bogus", true},
		{"temp:vrm", "vrm", true},
		{"temp:", "", false},
		{"temp:CPU", "", false},                       // 必须小写，避免出现两个等价指标名
		{"temp:cpu2", "cpu2", true},                   //
		{"temp:a b", "", false},                       // 不允许空白
		{"temp:aaaaaaaaaaaaaaaaaaaaaaaaa", "", false}, // 超长
		{"tempcpU", "", false},
		{"cpu", "", false},
		{"gpu_temp", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		kind, ok := tempMetricKind(c.in)
		if kind != c.kind || ok != c.ok {
			t.Errorf("tempMetricKind(%q) = (%q,%v)，期望 (%q,%v)", c.in, kind, ok, c.kind, c.ok)
		}
	}
}

func TestTempValueGlobalAndPerKind(t *testing.T) {
	sm := &Sample{
		Temps: []TempStat{
			{Name: "coretemp", Kind: TempKindCPU, TempC: 58},
			{Name: "core-1", Kind: TempKindCPU, TempC: 72.4},
			{Name: "nvme", Kind: TempKindDisk, TempC: 88.5},
			{Name: "acpitz", Kind: TempKindBoard, TempC: 30},
		},
	}

	// 全部类别 → 最高温（温度是"越高越坏"，必须取 max 而不是平均）
	if v, ok := tempValue(sm, ""); !ok || v != 88.5 {
		t.Errorf("全局最高温应为 88.5，实际 (%v,%v)", v, ok)
	}
	if v, ok := tempValue(sm, TempKindCPU); !ok || v != 72.4 {
		t.Errorf("CPU 最高温应为 72.4，实际 (%v,%v)", v, ok)
	}
	if _, ok := tempValue(sm, TempKindNIC); ok {
		t.Error("没有网卡传感器时应返回 ok=false")
	}
}

func TestTempValueFiltersBadReadings(t *testing.T) {
	sm := &Sample{Temps: []TempStat{
		{Name: "zero", Kind: TempKindCPU, TempC: 0},
		{Name: "neg", Kind: TempKindCPU, TempC: -5},
		{Name: "garbage", Kind: TempKindCPU, TempC: 255},
		{Name: "good", Kind: TempKindCPU, TempC: 61.2},
	}}
	v, ok := tempValue(sm, "")
	if !ok || v != 61.2 {
		t.Errorf("异常读数应被丢弃，应为 61.2，实际 (%v,%v)", v, ok)
	}
}

// 老版本 agent 只上报 cpu.temp_c / gpu[].temp_c，升级期看板不能直接空白。
func TestTempValueLegacyFallback(t *testing.T) {
	sm := &Sample{
		CPU: &CPUStat{TempC: 55.5},
		GPU: []GPUStat{{Index: 0, TempC: 71}},
	}
	if v, ok := tempValue(sm, TempKindCPU); !ok || v != 55.5 {
		t.Errorf("应从 cpu.temp_c 回退，实际 (%v,%v)", v, ok)
	}
	if v, ok := tempValue(sm, TempKindGPU); !ok || v != 71 {
		t.Errorf("应从 gpu[].temp_c 回退，实际 (%v,%v)", v, ok)
	}
	if v, ok := tempValue(sm, ""); !ok || v != 71 {
		t.Errorf("全局回退应取两者最大值 71，实际 (%v,%v)", v, ok)
	}

	// 只有 max_temp_c 的极端情况
	sm2 := &Sample{MaxTempC: 66}
	if v, ok := tempValue(sm2, ""); !ok || v != 66 {
		t.Errorf("应从 max_temp_c 回退，实际 (%v,%v)", v, ok)
	}
}

func TestTempValueEmpty(t *testing.T) {
	if _, ok := tempValue(&Sample{}, ""); ok {
		t.Error("没有任何温度数据时应返回 ok=false")
	}
}

func TestSummarizeTemps(t *testing.T) {
	if got := SummarizeTemps(&Sample{}); got != nil {
		t.Errorf("没有温度传感器时应返回 nil，实际 %+v", got)
	}

	sm := &Sample{Temps: []TempStat{
		{Name: "Package id 0", Kind: TempKindCPU, TempC: 58},
		{Name: "Composite", Kind: TempKindDisk, TempC: 41},
		{Name: "Sensor 1", Kind: TempKindDisk, TempC: 88.5},
		{Name: "acpitz", Kind: TempKindBoard, TempC: 30},
		{Name: "garbage", Kind: TempKindOther, TempC: 255},
	}}
	got := SummarizeTemps(sm)
	if got == nil {
		t.Fatal("应返回概览")
	}
	if got.MaxC != 88.5 || got.MaxName != "Sensor 1" || got.MaxKind != TempKindDisk {
		t.Errorf("最高温定位错误: %+v", got)
	}
	if got.SensorNum != 5 {
		t.Errorf("传感器数量应为 5，实际 %d", got.SensorNum)
	}
	if got.ByKind[TempKindCPU] != 58 || got.ByKind[TempKindDisk] != 88.5 || got.ByKind[TempKindBoard] != 30 {
		t.Errorf("按类别聚合错误: %+v", got.ByKind)
	}
	// 255 是寄存器解析错误，不能进 by_kind，否则卡片会显示一个假的红条
	if _, ok := got.ByKind[TempKindOther]; ok {
		t.Errorf("异常读数不该进入 by_kind: %+v", got.ByKind)
	}
}

func TestSummarizeTempsOnlyMaxTempC(t *testing.T) {
	got := SummarizeTemps(&Sample{MaxTempC: 47})
	if got == nil || got.MaxC != 47 {
		t.Fatalf("只有 max_temp_c 时也应给出概览，实际 %+v", got)
	}
	if got.ByKind != nil {
		t.Errorf("没有明细时 by_kind 应为 nil，实际 %+v", got.ByKind)
	}
}

func TestResolveMetricTemperature(t *testing.T) {
	sm := &Sample{
		Temps: []TempStat{
			{Name: "cpu", Kind: TempKindCPU, TempC: 60},
			{Name: "disk", Kind: TempKindDisk, TempC: 91.5},
		},
	}
	cases := []struct {
		metric string
		want   float64
	}{
		{"temp", 91.5},
		{"temp:cpu", 60},
		{"temp:disk", 91.5},
	}
	for _, c := range cases {
		v, ok := resolveMetric(sm, c.metric, "")
		if !ok || v != c.want {
			t.Errorf("resolveMetric(%q) = (%v,%v)，期望 %v", c.metric, v, ok, c.want)
		}
	}
	if _, ok := resolveMetric(sm, "temp:nic", ""); ok {
		t.Error("没有网卡温度时应返回 ok=false")
	}
	// 已有的指标不能被温度逻辑碰坏
	if v, ok := resolveMetric(sm, "cpu", ""); ok || v != 0 {
		t.Errorf("未上报 CPU 占用时 cpu 指标应为 false，实际 (%v,%v)", v, ok)
	}
}

func TestMetricUnitTemperature(t *testing.T) {
	for _, m := range []string{"temp", "temp:cpu", "temp:disk", "temp:other"} {
		if got := metricUnit(m); got != "°C" {
			t.Errorf("metricUnit(%q) = %q，期望 °C", m, got)
		}
	}
	// 不能误伤名字里含 temp 的其他指标
	if got := metricUnit("gpu_temp"); got != "°C" {
		t.Errorf("metricUnit(gpu_temp) = %q，期望 °C", got)
	}
	if got := metricUnit("temp_bogus"); got != "" {
		t.Errorf("metricUnit(temp_bogus) = %q，期望空", got)
	}
}
