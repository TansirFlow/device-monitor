package main

// CPU 拓扑解析的单元测试。
//
// 用假 sysfs 树 + 假 /proc/cpuinfo 覆盖真实世界里容易踩的几种机型：
// 单路超线程、双路、ARM 大小核（core_id 跨簇重复）、CPU 离线、
// 以及最关键的一条——**识别不出物理核时绝不能拿线程数冒充核心数**。
//
// 这些场景在真机上很难凑齐（需要同时有 8 核 16 线程的笔记本、双路服务器、
// 树莓派），所以用 fixture。buildSysfs / useSysRoot 定义在 tempsysfs_test.go 里。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cpuTopoFiles 生成一棵 sysfs 树：n 个逻辑处理器，
// 每 logicalPerCore 个共享一个物理核心，每 coresPerSocket 个核心共享一个插槽。
// online 为空表示不写 online 文件。
func cpuTopoFiles(n, logicalPerCore, coresPerSocket int, online string) map[string]string {
	files := map[string]string{"devices/system/cpu/": ""}
	for i := 0; i < n; i++ {
		core := (i / logicalPerCore) % coresPerSocket
		pkg := i / (logicalPerCore * coresPerSocket)
		dir := fmt.Sprintf("devices/system/cpu/cpu%d/topology/", i)
		files[dir] = ""
		files[dir+"core_id"] = fmt.Sprint(core)
		files[dir+"physical_package_id"] = fmt.Sprint(pkg)
	}
	if online != "" {
		files["devices/system/cpu/online"] = online
	}
	return files
}

// useCPUInfo 把 /proc/cpuinfo 指到一个临时文件上；测试结束自动还原。
func useCPUInfo(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写 cpuinfo fixture: %v", err)
	}
	old := procCPUInfoPath
	procCPUInfoPath = p
	t.Cleanup(func() { procCPUInfoPath = old })
}

// useMissingCPUInfo 让 /proc/cpuinfo 读不到（模拟精简容器）。
func useMissingCPUInfo(t *testing.T) {
	t.Helper()
	old := procCPUInfoPath
	procCPUInfoPath = filepath.Join(t.TempDir(), "不存在")
	t.Cleanup(func() { procCPUInfoPath = old })
}

// ---------- sysfs 来源 ----------

// 单路 8 核 16 线程：这是最典型的家用/开发机，也是当初报错的那类机器。
func TestCPUTopoSysfsSingleSocketHT(t *testing.T) {
	useSysRoot(t, buildSysfs(t, cpuTopoFiles(16, 2, 8, "0-15")))

	got := cpuTopoSysfs()
	want := cputopo{Cores: 8, Threads: 16, Sockets: 1}
	if got != want {
		t.Fatalf("8 核 16 线程：得到 %+v，期望 %+v", got, want)
	}
}

// 双路 2 × 4 核 × 2 线程 = 8 物理核 / 16 线程，但有两个插槽。
// 光看"8 核"分不出是 1 路 ×8 还是 2 路 ×4，所以 Sockets 必须单独统计。
func TestCPUTopoSysfsDualSocket(t *testing.T) {
	useSysRoot(t, buildSysfs(t, cpuTopoFiles(16, 2, 4, "0-15")))

	got := cpuTopoSysfs()
	want := cputopo{Cores: 8, Threads: 16, Sockets: 2}
	if got != want {
		t.Fatalf("双路 8 核 16 线程：得到 %+v，期望 %+v", got, want)
	}
}

// 无超线程：核数 == 线程数，看板应显示成"N 核"。
func TestCPUTopoSysfsNoSMT(t *testing.T) {
	useSysRoot(t, buildSysfs(t, cpuTopoFiles(8, 1, 8, "0-7")))

	got := cpuTopoSysfs()
	want := cputopo{Cores: 8, Threads: 8, Sockets: 1}
	if got != want {
		t.Fatalf("8 核 8 线程：得到 %+v，期望 %+v", got, want)
	}
}

// ARM 大小核：两个簇里 core_id 都从 0 开始，且没有 physical_package_id。
// 如果把 core_id 单独当键，8 个物理核会被并成 4 个——这就是要 cluster_id 的原因。
func TestCPUTopoSysfsARMClusters(t *testing.T) {
	files := map[string]string{
		"devices/system/cpu/":                         "",
		"devices/system/cpu/online":                   "0-7",
		"devices/system/cpu/cpu0/topology/":           "",
		"devices/system/cpu/cpu0/topology/core_id":    "0",
		"devices/system/cpu/cpu0/topology/cluster_id": "0",
		"devices/system/cpu/cpu1/topology/":           "",
		"devices/system/cpu/cpu1/topology/core_id":    "1",
		"devices/system/cpu/cpu1/topology/cluster_id": "0",
		"devices/system/cpu/cpu2/topology/":           "",
		"devices/system/cpu/cpu2/topology/core_id":    "2",
		"devices/system/cpu/cpu2/topology/cluster_id": "0",
		"devices/system/cpu/cpu3/topology/":           "",
		"devices/system/cpu/cpu3/topology/core_id":    "3",
		"devices/system/cpu/cpu3/topology/cluster_id": "0",
		"devices/system/cpu/cpu4/topology/":           "",
		"devices/system/cpu/cpu4/topology/core_id":    "0",
		"devices/system/cpu/cpu4/topology/cluster_id": "1",
		"devices/system/cpu/cpu5/topology/":           "",
		"devices/system/cpu/cpu5/topology/core_id":    "1",
		"devices/system/cpu/cpu5/topology/cluster_id": "1",
		"devices/system/cpu/cpu6/topology/":           "",
		"devices/system/cpu/cpu6/topology/core_id":    "2",
		"devices/system/cpu/cpu6/topology/cluster_id": "1",
		"devices/system/cpu/cpu7/topology/":           "",
		"devices/system/cpu/cpu7/topology/core_id":    "3",
		"devices/system/cpu/cpu7/topology/cluster_id": "1",
	}
	useSysRoot(t, buildSysfs(t, files))

	got := cpuTopoSysfs()
	want := cputopo{Cores: 8, Threads: 8, Sockets: 1}
	if got != want {
		t.Fatalf("ARM 双簇 8 核：得到 %+v，期望 %+v（core_id 跨簇重复，必须带上 cluster_id）", got, want)
	}
}

// CPU 被离线后目录还在，在线列表才是可信的线程数。
func TestCPUTopoSysfsOnlineListWins(t *testing.T) {
	useSysRoot(t, buildSysfs(t, cpuTopoFiles(16, 2, 8, "0-3,8-11")))

	got := cpuTopoSysfs()
	if got.Threads != 8 {
		t.Errorf("在线列表 0-3,8-11 应得到 8 个线程，得到 %d", got.Threads)
	}
	if got.Cores != 8 {
		t.Errorf("物理核数不应受在线列表影响，得到 %d", got.Cores)
	}
}

// 没有 online 文件时退回目录个数，不能因为缺一个文件就把线程数变成 0。
func TestCPUTopoSysfsDirCountFallback(t *testing.T) {
	useSysRoot(t, buildSysfs(t, cpuTopoFiles(12, 2, 6, "")))

	got := cpuTopoSysfs()
	if got.Threads != 12 {
		t.Errorf("无 online 文件时应退回目录数 12，得到 %d", got.Threads)
	}
	if got.Cores != 6 {
		t.Errorf("物理核应为 6，得到 %d", got.Cores)
	}
}

// 完全没有 sysfs（未挂载 /sys 的容器）：全部保持零值，由上层决定降级到哪。
func TestCPUTopoSysfsAbsent(t *testing.T) {
	useSysRoot(t, t.TempDir())

	if got := cpuTopoSysfs(); got != (cputopo{}) {
		t.Fatalf("没有 sysfs 时应返回零值，得到 %+v", got)
	}
}

// 目录在但读不到 core_id（部分 ARM）：不猜核心数，只给线程数。
func TestCPUTopoSysfsNoCoreID(t *testing.T) {
	files := map[string]string{
		"devices/system/cpu/":               "",
		"devices/system/cpu/online":         "0-3",
		"devices/system/cpu/cpu0/topology/": "",
		"devices/system/cpu/cpu1/topology/": "",
		"devices/system/cpu/cpu2/topology/": "",
		"devices/system/cpu/cpu3/topology/": "",
	}
	useSysRoot(t, buildSysfs(t, files))

	got := cpuTopoSysfs()
	if got.Cores != 0 {
		t.Errorf("拿不到 core_id 时不应编造核心数，得到 %d", got.Cores)
	}
	if got.Threads != 4 {
		t.Errorf("线程数应为 4，得到 %d", got.Threads)
	}
}

// ---------- CPU 列表解析 ----------

func TestCountCPUList(t *testing.T) {
	cases := []struct {
		spec string
		want int
		ok   bool
	}{
		{"0-15", 16, true},
		{"0", 1, true},
		{"7", 1, true},
		{"0-3,8-11", 8, true},
		{"0-2,4", 4, true},
		{" 0-1 , 3 ", 3, true},
		{"", 0, false},
		{"abc", 0, false},
		{"5-3", 0, false},        // 区间倒置
		{"-3", 0, false},         // 少了起点
		{"0-", 0, false},         // 少了终点
		{"1,,2", 0, false},       // 空段
		{"0--3", 0, false},       // 畸形
		{"0-99999999", 0, false}, // 超出合理上限
	}
	for _, c := range cases {
		got, ok := countCPUList(c.spec)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("countCPUList(%q) = (%d, %v)，期望 (%d, %v)", c.spec, got, ok, c.want, c.ok)
		}
	}
}

// ---------- /proc/cpuinfo 来源 ----------

// x86 的一段 cpuinfo：2 路 × 4 核 × 2 线程。
func x86CPUInfo(threads, logicalPerCore, coresPerSocket int) string {
	var b strings.Builder
	for i := 0; i < threads; i++ {
		fmt.Fprintf(&b, "processor\t: %d\n", i)
		b.WriteString("vendor_id\t: GenuineIntel\n")
		fmt.Fprintf(&b, "physical id\t: %d\n", i/(logicalPerCore*coresPerSocket))
		fmt.Fprintf(&b, "core id\t\t: %d\n", (i/logicalPerCore)%coresPerSocket)
		b.WriteString("cpu MHz\t\t: 3200.000\n\n")
	}
	return b.String()
}

func TestParseCPUInfo(t *testing.T) {
	got := parseCPUInfo(x86CPUInfo(16, 2, 4))
	want := cputopo{Cores: 8, Threads: 16, Sockets: 2}
	if got != want {
		t.Fatalf("x86 双路 8 核 16 线程：得到 %+v，期望 %+v", got, want)
	}
}

// ARM 的 cpuinfo 没有 core id，只有 processor 段：线程数可信，核心数不可信。
func TestParseCPUInfoARM(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, "processor\t: %d\n", i)
		b.WriteString("BogoMIPS\t: 38.40\n")
		b.WriteString("CPU part\t: 0xd03\n\n")
	}
	got := parseCPUInfo(b.String())
	if got.Cores != 0 {
		t.Errorf("ARM cpuinfo 无 core id，不应编造核心数，得到 %d", got.Cores)
	}
	if got.Threads != 4 {
		t.Errorf("线程数应为 4，得到 %d", got.Threads)
	}
}

func TestParseCPUInfoGarbage(t *testing.T) {
	for _, s := range []string{"", "\n\n", "hello world", "processor: x\n"} {
		if got := parseCPUInfo(s); got != (cputopo{}) {
			t.Errorf("parseCPUInfo(%q) 期望零值，得到 %+v", s, got)
		}
	}
}

// ---------- Linux 多来源降级链 ----------

// sysfs 可用时以 sysfs 为准，不读 cpuinfo。
func TestLinuxCPUTopoPrefersSysfs(t *testing.T) {
	useSysRoot(t, buildSysfs(t, cpuTopoFiles(16, 2, 8, "0-15")))
	useCPUInfo(t, x86CPUInfo(4, 1, 4)) // 故意给一份矛盾的数据

	got := linuxCPUTopo(17) // /proc/stat 有 17 行（1 个总体 + 16 个核）
	want := cputopo{Cores: 8, Threads: 16, Sockets: 1}
	if got != want {
		t.Fatalf("应以 sysfs 为准：得到 %+v，期望 %+v", got, want)
	}
}

// sysfs 没挂载时降级到 /proc/cpuinfo。
func TestLinuxCPUTopoFallsBackToCPUInfo(t *testing.T) {
	useSysRoot(t, t.TempDir())
	useCPUInfo(t, x86CPUInfo(16, 2, 8))

	got := linuxCPUTopo(17)
	want := cputopo{Cores: 8, Threads: 16, Sockets: 1}
	if got != want {
		t.Fatalf("应降级到 /proc/cpuinfo：得到 %+v，期望 %+v", got, want)
	}
}

// 两个来源都没有时，只能从 /proc/stat 的行数得到线程数，核心数如实留空。
func TestLinuxCPUTopoFallsBackToProcStatOnly(t *testing.T) {
	useSysRoot(t, t.TempDir())
	useMissingCPUInfo(t)

	got := linuxCPUTopo(17)
	if got.Threads != 16 {
		t.Errorf("应从 /proc/stat 行数得到 16 个线程，得到 %d", got.Threads)
	}
	if got.Cores != 0 {
		t.Fatalf("三个来源都拿不到物理核时应留空，绝不能退回线程数，得到 %d", got.Cores)
	}
	if got.Sockets != 0 {
		t.Errorf("插槽数同样应留空，得到 %d", got.Sockets)
	}
}

// ---------- setTopo 的不变式 ----------

func TestSetTopoInvariants(t *testing.T) {
	// 什么都不知道：线程兜底到 NumCPU，核心保持 0
	var cs CPUStat
	cs.setTopo(cputopo{})
	if cs.Threads != runtime.NumCPU() {
		t.Errorf("线程应兜底到 NumCPU()=%d，得到 %d", runtime.NumCPU(), cs.Threads)
	}
	if cs.Cores != 0 {
		t.Errorf("核心数未知时必须保持 0，得到 %d", cs.Cores)
	}

	// 只有核心数（异常情况）：插槽补 1，线程仍兜底
	cs = CPUStat{}
	cs.setTopo(cputopo{Cores: 8})
	if cs.Cores != 8 || cs.Sockets != 1 || cs.Threads != runtime.NumCPU() {
		t.Errorf("只给核心数时应补默认插槽，得到 %+v", cs)
	}

	// 完整拓扑：原样写入，不做任何"聪明"的推导
	cs = CPUStat{}
	cs.setTopo(cputopo{Cores: 8, Threads: 16, Sockets: 2})
	if cs.Cores != 8 || cs.Threads != 16 || cs.Sockets != 2 {
		t.Errorf("完整拓扑应原样写入，得到 %+v", cs)
	}
}

// 回归：8 核 16 线程的机器上，绝不能再报成 16 核。
func TestSetTopoDoesNotMistakeThreadsForCores(t *testing.T) {
	useSysRoot(t, buildSysfs(t, cpuTopoFiles(16, 2, 8, "0-15")))

	var cs CPUStat
	cs.setTopo(linuxCPUTopo(17))
	if cs.Cores == cs.Threads {
		t.Fatalf("8 核 16 线程被报成了 %d 核 %d 线程", cs.Cores, cs.Threads)
	}
	if cs.Cores != 8 || cs.Threads != 16 {
		t.Fatalf("期望 8 核 16 线程，得到 %d 核 %d 线程", cs.Cores, cs.Threads)
	}
}
