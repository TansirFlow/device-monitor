package main

// CPU 拓扑解析：回答「这台机器是 8 核 16 线程，还是 16 核」。
//
// 为什么值得单独立一个文件：runtime.NumCPU()、/proc/stat 里 cpuN 的行数、
// /proc/cpuinfo 里 processor 的块数，返回的**全都是逻辑处理器数**
// （开了超线程就是线程数）。把它当核心数报出去，会在 8 核 16 线程的机器上
// 显示成「16 核」——容量规划时正好差一倍，而且是"看起来很正常"的那种错。
//
// 物理核心必须另外去问内核：
//
//	Linux    /sys/devices/system/cpu/cpuN/topology/{core_id,physical_package_id}
//	         唯一的 (package_id, core_id) 组合数 = 物理核心数
//	Windows  GetLogicalProcessorInformationEx(RelationProcessorCore) 的记录条数
//
// 与 tempsysfs.go 同理，本文件**不加 build tag**：解析规则是纯逻辑
// （在一棵目录树上按约定文件名读文本），用假 sysfs 树就能在任意平台上被真实覆盖。

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// cputopo 是 CPU 的物理拓扑。零值表示「没识别出来」，不是「0 个」。
type cputopo struct {
	Cores   int // 物理核心总数（多路插槽累加）
	Threads int // 逻辑处理器数（含超线程）
	Sockets int // 物理插槽（路）数
}

// cpuIndexCap 是 CPU 编号的合理上限，用来挡住畸形列表（如 "0-4294967295"）
// 把线程数撑成天文数字——看板上一个荒谬的数值比"未知"更具误导性。
const cpuIndexCap = 1 << 16

// setTopo 把拓扑写进 CPUStat。两条不变式：
//
//   - Threads 兜底到 runtime.NumCPU()：它一定拿得到，且就是逻辑处理器数；
//   - Cores 识别不出来就保持 0，让看板只显示线程数。
//     用线程数冒充核心数正是本次要修掉的 bug，不能在兜底路径里再犯一次。
func (cs *CPUStat) setTopo(t cputopo) {
	if t.Threads <= 0 {
		t.Threads = runtime.NumCPU()
	}
	if t.Sockets <= 0 && t.Cores > 0 {
		t.Sockets = 1
	}
	cs.Cores = t.Cores
	cs.Threads = t.Threads
	cs.Sockets = t.Sockets
}

// cpuTopoSysfs 从 sysfs 解析 CPU 拓扑（Linux 主来源）。
//
// 每个逻辑处理器在 /sys/devices/system/cpu/ 下有一个 cpuN 目录：
//
//	cpuN/topology/core_id              该物理核心在插槽内的编号
//	cpuN/topology/physical_package_id  插槽号（部分 ARM 上缺失，视为 0）
//	cpuN/topology/cluster_id           ARM 的簇号（x86 上不存在）
//
// 统计唯一的 (package_id, cluster_id, core_id) 组合就得到物理核心数。
// core_id 只保证在同一个簇内唯一，因此 ARM 上必须把 cluster_id 一起算进键，
// 否则大核簇与小核簇的 core_id 0 会被并成一个核心。
//
// 直接数 cpuN 目录个数会把超线程多算一倍——那正是要避免的错误。
func cpuTopoSysfs() cputopo {
	var t cputopo
	dirs, err := filepath.Glob(sysfsPath("devices", "system", "cpu", "cpu[0-9]*"))
	if err != nil || len(dirs) == 0 {
		return t
	}

	cores := map[string]bool{}
	sockets := map[string]bool{}
	for _, dir := range dirs {
		topo := filepath.Join(dir, "topology")
		core := readTrimmed(filepath.Join(topo, "core_id"))
		if core == "" {
			continue // 读不到 core_id 就不猜，宁可少报
		}
		pkg := readTrimmed(filepath.Join(topo, "physical_package_id"))
		if pkg == "" {
			pkg = "0"
		}
		cluster := readTrimmed(filepath.Join(topo, "cluster_id"))
		cores[pkg+"\x00"+cluster+"\x00"+core] = true
		sockets[pkg] = true
	}

	t.Cores = len(cores)
	t.Sockets = len(sockets)
	t.Threads = onlineCPUCount(len(dirs))
	return t
}

// onlineCPUCount 取「在线逻辑处理器」数量。
//
// 优先读 /sys/devices/system/cpu/online（形如 "0-15"、"0-3,8-11"），
// 它比数目录更准：CPU 被离线（热插拔、isolcpus、SMT 关闭到一半）后目录仍在，
// 但已不该算进可调度线程数里。读不到就退回目录个数。
func onlineCPUCount(dirNum int) int {
	if n, ok := countCPUList(readTrimmed(sysfsPath("devices", "system", "cpu", "online"))); ok && n > 0 {
		return n
	}
	return dirNum
}

// countCPUList 解析内核的 CPU 列表格式："0-15"、"0-3,8-11"、"7"。
// 解析失败返回 ok=false，由调用方决定退回哪个来源（不返回 0 冒充真实值）。
func countCPUList(spec string) (int, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, false
	}
	total := 0
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, false
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, ok := parseCPUIndex(lo)
		if !ok {
			return 0, false
		}
		if !isRange {
			total++
			continue
		}
		b, ok := parseCPUIndex(hi)
		if !ok || b < a {
			return 0, false
		}
		total += b - a + 1
		if total > cpuIndexCap {
			return 0, false
		}
	}
	if total == 0 {
		return 0, false
	}
	return total, true
}

func parseCPUIndex(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// linuxCPUTopo 按信息量从高到低汇总 Linux 上的三个拓扑来源：
//
//  1. sysfs topology —— 唯一能同时给出物理核心数与插槽数的来源
//  2. /proc/cpuinfo  —— sysfs 没挂载时的兜底（精简容器、部分 WSL、老内核）
//  3. /proc/stat 行数 —— 只能给出逻辑处理器数，垫底
//
// 物理核心数三个来源都可能拿不到，这时 Cores 保持 0、看板只显示线程数，
// 而不是退回逻辑处理器数——那正是本次要修掉的错误。
//
// 之所以放在本文件而不是 collect_linux.go：它只是一段优先级策略，不含任何系统调用
// （真正读文件的是 cpuTopoSysfs / cpuTopoProc），放这里就能在任意平台上被单元测试覆盖。
func linuxCPUTopo(procStatLines int) cputopo {
	t := cpuTopoSysfs()
	if t.Cores == 0 || t.Sockets == 0 {
		if p := cpuTopoProc(); p.Cores > 0 {
			if t.Cores == 0 {
				t.Cores = p.Cores
			}
			if t.Sockets == 0 {
				t.Sockets = p.Sockets
			}
			if t.Threads == 0 {
				t.Threads = p.Threads
			}
		}
	}
	if t.Threads == 0 && procStatLines > 1 {
		t.Threads = procStatLines - 1
	}
	return t
}

// procCPUInfoPath 是 /proc/cpuinfo 的路径。与 sysRoot 同理做成变量，便于测试注入。
var procCPUInfoPath = "/proc/cpuinfo"

// cpuTopoProc 从 /proc/cpuinfo 解析拓扑。
//
// 作为 sysfs 不可用时的兜底来源。每个逻辑处理器一段，段内两行是拓扑信息：
//
//	processor   : 0
//	physical id : 0     <- 插槽号，部分 ARM/x86 没有这个字段，视为 0
//	core id     : 0     <- 该插槽内的物理核心号
//
// 与 sysfs 版同理，物理核心 = 唯一的 (physical id, core id) 组合。
func cpuTopoProc() cputopo {
	b, err := os.ReadFile(procCPUInfoPath)
	if err != nil {
		return cputopo{}
	}
	return parseCPUInfo(string(b))
}

// parseCPUInfo 是 cpuTopoProc 的纯函数部分，便于直接喂字符串做单元测试。
func parseCPUInfo(data string) cputopo {
	var t cputopo
	cores := map[string]bool{}
	sockets := map[string]bool{}
	var pkg, core string
	threads := 0

	// flush 结算刚读完的一段；没有 core id 的段（ARM）不计入核心数。
	flush := func() {
		if core != "" {
			p := pkg
			if p == "" {
				p = "0"
			}
			cores[p+"\x00"+core] = true
			sockets[p] = true
		}
		pkg, core = "", ""
	}

	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "processor":
			flush() // 新的一段开始
			if _, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				threads++
			}
		case "physical id":
			pkg = strings.TrimSpace(v)
		case "core id":
			core = strings.TrimSpace(v)
		}
	}
	flush()

	t.Cores = len(cores)
	t.Sockets = len(sockets)
	t.Threads = threads
	return t
}
