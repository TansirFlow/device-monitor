package main

// 温度采集的单元测试。
//
// 这套测试的重点是 sysfs 解析规则：芯片型号归类、thermal_zone 去重、
// milli-degree 换算、trip_point critical 解析。这些逻辑在真实硬件上
// 很难构造全（需要 Intel CPU + NVMe + AMD GPU + ARM 板卡各一台），
// 所以在 testdata 里搭一棵假的 sysfs 树来验证。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildSysfs 在临时目录里搭出一棵 sysfs 树。
// map 的 key 是相对路径（用 / 分隔），以 / 结尾表示目录；
// value 是文件内容。内容写成 "" 会得到一个空文件（用于测"读不到"的情况）。
func buildSysfs(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatalf("建目录 %s: %v", rel, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建父目录 %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写文件 %s: %v", rel, err)
		}
	}
	return root
}

// useSysRoot 把解析器的根指到 fixture 上；测试结束自动还原。
// 注意这会改包级变量，所以本文件的测试不能并行跑。
func useSysRoot(t *testing.T, root string) {
	t.Helper()
	old := sysRoot
	sysRoot = root
	t.Cleanup(func() { sysRoot = old })
}

func findTemp(list []TempStat, source string) (TempStat, bool) {
	for _, s := range list {
		if s.Source == source {
			return s, true
		}
	}
	return TempStat{}, false
}

// x86 机器：coretemp + nvme + amdgpu + 未知芯片 + 一台 thermal_zone。
var x86Sysfs = map[string]string{
	// Intel CPU：两个通道，带 label / max / crit
	"class/hwmon/hwmon0/name":        "coretemp\n",
	"class/hwmon/hwmon0/temp1_input": "58000\n",
	"class/hwmon/hwmon0/temp1_label": "Package id 0\n",
	"class/hwmon/hwmon0/temp1_max":   "84000\n",
	"class/hwmon/hwmon0/temp1_crit":  "100000\n",
	"class/hwmon/hwmon0/temp2_input": "56000\n",
	"class/hwmon/hwmon0/temp2_label": "Core 0\n",
	"class/hwmon/hwmon0/temp2_crit":  "100000\n",
	// 下面三条都是"看起来像但必须被忽略"的文件
	"class/hwmon/hwmon0/temp3_input":     "0\n",     // 读数为 0 → 读不到
	"class/hwmon/hwmon0/temp_input":      "99999\n", // 没有通道号
	"class/hwmon/hwmon0/temp1_max_alarm": "1\n",     // 不是 _input

	// NVMe 固态盘
	"class/hwmon/hwmon1/name":         "nvme\n",
	"class/hwmon/hwmon1/temp1_input":  "41500\n",
	"class/hwmon/hwmon1/temp1_label":  "Composite\n",
	"class/hwmon/hwmon1/temp1_max":    "65850\n",
	"class/hwmon/hwmon1/temp1_crit":   "85250\n",
	"class/hwmon/hwmon1/temp10_input": "notanumber\n", // 内容坏掉

	// 显卡
	"class/hwmon/hwmon2/name":        "amdgpu\n",
	"class/hwmon/hwmon2/temp1_input": "52000\n",
	"class/hwmon/hwmon2/temp1_label": "edge\n",
	"class/hwmon/hwmon2/temp1_crit":  "100000\n",

	// 不在映射表里的芯片 → other
	"class/hwmon/hwmon3/name":        "mystery_chip\n",
	"class/hwmon/hwmon3/temp1_input": "25000\n",

	// 没有 name 文件的芯片：应退回用目录名当芯片名，而不是整块丢掉
	"class/hwmon/hwmon4/name":        "",
	"class/hwmon/hwmon4/temp1_input": "33000\n",

	// thermal：x86_pkg_temp 与 hwmon 的 CPU 重复，acpitz 是 hwmon 没有的类别
	"class/thermal/thermal_zone0/type":              "x86_pkg_temp\n",
	"class/thermal/thermal_zone0/temp":              "57000\n",
	"class/thermal/thermal_zone0/trip_point_0_type": "passive\n",
	"class/thermal/thermal_zone0/trip_point_0_temp": "95000\n",
	"class/thermal/thermal_zone0/trip_point_1_type": "critical\n",
	"class/thermal/thermal_zone0/trip_point_1_temp": "100000\n",
	"class/thermal/thermal_zone1/type":              "acpitz\n",
	"class/thermal/thermal_zone1/temp":              "30500\n",
}

func TestReadHwmonSensorsX86(t *testing.T) {
	useSysRoot(t, buildSysfs(t, x86Sysfs))

	got := readHwmonSensors()
	if len(got) != 6 {
		t.Fatalf("期望 6 个 hwmon 传感器，实际 %d 个: %+v", len(got), got)
	}

	pkg, ok := findTemp(got, "hwmon:coretemp/temp1")
	if !ok {
		t.Fatal("缺少 coretemp/temp1")
	}
	if pkg.Kind != TempKindCPU || pkg.Name != "CPU · Package id 0" {
		t.Errorf("coretemp 归类错误: kind=%q name=%q", pkg.Kind, pkg.Name)
	}
	if pkg.TempC != 58 || pkg.MaxC != 84 || pkg.CritC != 100 {
		t.Errorf("coretemp 读数/阈值错误: %+v", pkg)
	}

	nvme, ok := findTemp(got, "hwmon:nvme/temp1")
	if !ok {
		t.Fatal("缺少 nvme/temp1")
	}
	if nvme.Kind != TempKindDisk {
		t.Errorf("nvme 应归为硬盘，实际 %q", nvme.Kind)
	}
	// 65850/1000 = 65.85 → 保留 1 位小数应进位到 65.9
	if nvme.TempC != 41.5 || nvme.MaxC != 65.9 || nvme.CritC != 85.3 {
		t.Errorf("nvme 毫摄氏度换算/取整错误: %+v", nvme)
	}

	if g, ok := findTemp(got, "hwmon:amdgpu/temp1"); !ok || g.Kind != TempKindGPU {
		t.Errorf("amdgpu 应归为 GPU: ok=%v kind=%q", ok, g.Kind)
	}

	unknown, ok := findTemp(got, "hwmon:mystery_chip/temp1")
	if !ok || unknown.Kind != TempKindOther {
		t.Errorf("未知芯片应归为 other: ok=%v kind=%q", ok, unknown.Kind)
	}
	// 没有 label 时给一个可读的兜底名
	if unknown.Name != "其他 · 传感器 1" {
		t.Errorf("缺 label 时的兜底名不对: %q", unknown.Name)
	}

	nameless, ok := findTemp(got, "hwmon:hwmon4/temp1")
	if !ok {
		t.Fatal("没有 name 文件的芯片不应被整块丢弃")
	}
	if nameless.TempC != 33 {
		t.Errorf("无名芯片读数错误: %+v", nameless)
	}

	// 三种"像但不是"的文件都不能产出传感器
	for _, bad := range []string{"hwmon:coretemp/temp3", "hwmon:coretemp/temp", "hwmon:nvme/temp10"} {
		if s, ok := findTemp(got, bad); ok {
			t.Errorf("%s 不该被识别为传感器: %+v", bad, s)
		}
	}
}

func TestCollectTempsSysfsDedup(t *testing.T) {
	useSysRoot(t, buildSysfs(t, x86Sysfs))

	var errs []string
	got := collectTempsSysfs(&errs)
	if len(errs) != 0 {
		t.Errorf("有传感器时不该报降级: %v", errs)
	}

	// hwmon 6 个 + thermal 里 board 是 hwmon 没有的类别
	if len(got) != 7 {
		for _, s := range got {
			t.Logf("  %-28s %-7s %s %v", s.Source, s.Kind, s.Name, s.TempC)
		}
		t.Fatalf("期望 7 个传感器（6 hwmon + 1 thermal），实际 %d 个", len(got))
	}

	// x86_pkg_temp 与 coretemp 同属 CPU，必须被去掉，否则每台 x86 都会重复报一次
	if s, ok := findTemp(got, "thermal:thermal_zone0"); ok {
		t.Errorf("已覆盖的类别不该再补 thermal：%+v", s)
	}

	board, ok := findTemp(got, "thermal:thermal_zone1")
	if !ok {
		t.Fatal("thermal 里的 acpitz 应被保留（hwmon 没有覆盖主板类别）")
	}
	if board.Kind != TempKindBoard || board.Name != "主板 · acpitz" || board.TempC != 30.5 {
		t.Errorf("acpitz 归类错误: %+v", board)
	}
	if board.MaxC != 0 || board.CritC != 0 {
		t.Errorf("acpitz 没有阈值文件时应为 0: %+v", board)
	}
}

func TestCollectTempsSysfsEmptyReportsDegrade(t *testing.T) {
	useSysRoot(t, buildSysfs(t, map[string]string{"class/": ""}))

	var errs []string
	got := collectTempsSysfs(&errs)
	if len(got) != 0 {
		t.Fatalf("空 sysfs 不该产出传感器: %+v", got)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "temp:") {
		t.Fatalf("空 sysfs 应给出一条可操作的降级说明，实际: %v", errs)
	}
}

// ARM 板卡：只有 thermal_zone，没有任何 hwmon。
func TestCollectTempsSysfsARM(t *testing.T) {
	useSysRoot(t, buildSysfs(t, map[string]string{
		"class/thermal/thermal_zone0/type":              "cpu-thermal\n",
		"class/thermal/thermal_zone0/temp":              "45000\n",
		"class/thermal/thermal_zone0/trip_point_0_type": "critical\n",
		"class/thermal/thermal_zone0/trip_point_0_temp": "110000\n",
		"class/thermal/thermal_zone1/type":              "board-thermal\n",
		"class/thermal/thermal_zone1/temp":              "35000\n",
		// 没有 type 文件的热区：退回目录名，归入 other
		"class/thermal/thermal_zone2/temp": "28000\n",
	}))

	var errs []string
	got := collectTempsSysfs(&errs)
	if len(got) != 3 {
		t.Fatalf("期望 3 个热区，实际 %d: %+v", len(got), got)
	}

	cpu, ok := findTemp(got, "thermal:thermal_zone0")
	if !ok || cpu.Kind != TempKindCPU {
		t.Fatalf("cpu-thermal 应归为 CPU: %+v", cpu)
	}
	if cpu.TempC != 45 || cpu.CritC != 110 {
		t.Errorf("trip_point critical 未正确解析: %+v", cpu)
	}

	if b, ok := findTemp(got, "thermal:thermal_zone1"); !ok || b.Kind != TempKindBoard {
		t.Errorf("board-thermal 应归为主板: %+v", b)
	}
	unnamed, ok := findTemp(got, "thermal:thermal_zone2")
	if !ok || unnamed.Kind != TempKindOther || unnamed.Name != "其他 · thermal_zone2" {
		t.Errorf("无 type 的热区应退回目录名并归为 other: %+v", unnamed)
	}
}

func TestTempInputIndex(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"temp1_input", 1, true},
		{"temp12_input", 12, true},
		{"TEMP3_INPUT", 3, true},
		{"temp_input", 0, false},
		{"temp0_input", 0, false},
		{"tempX_input", 0, false},
		{"temp1_max", 0, false},
		{"temp1_label", 0, false},
		{"name", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		n, ok := tempInputIndex(c.in)
		if n != c.want || ok != c.ok {
			t.Errorf("tempInputIndex(%q) = (%d,%v)，期望 (%d,%v)", c.in, n, ok, c.want, c.ok)
		}
	}
}

func TestThermalTypeKind(t *testing.T) {
	cases := []struct{ in, want string }{
		{"x86_pkg_temp", TempKindCPU},
		{"cpu-thermal", TempKindCPU},
		{"soc_thermal", TempKindCPU},
		{"cluster0", TempKindCPU},
		{"amdgpu", TempKindGPU},
		{"nouveau", TempKindGPU},
		{"nvme", TempKindDisk},
		{"acpitz", TempKindBoard},
		{"pch_skylake", TempKindBoard},
		{"", TempKindOther},
		{"totally-unknown", TempKindOther},
	}
	for _, c := range cases {
		if got := thermalTypeKind(c.in); got != c.want {
			t.Errorf("thermalTypeKind(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestReadTempMilli(t *testing.T) {
	root := buildSysfs(t, map[string]string{
		"ok":      "41500\n",
		"round":   "65850\n", // 65.85 → 65.9
		"zero":    "0\n",
		"neg":     "-5000\n",
		"garbage": "abc\n",
		"empty":   "",
		"spaced":  "  58000  \n",
	})
	cases := []struct {
		file string
		want float64
	}{
		{"ok", 41.5}, {"round", 65.9}, {"zero", 0},
		{"neg", 0}, {"garbage", 0}, {"empty", 0},
		{"spaced", 58},
		{"missing", 0},
	}
	for _, c := range cases {
		got := readTempMilli(filepath.Join(root, c.file))
		if got != c.want {
			t.Errorf("readTempMilli(%s) = %v，期望 %v", c.file, got, c.want)
		}
	}
}
