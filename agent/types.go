package main

// Payload 是 agent 向 master 上报的唯一数据结构。
//
// 安全约定：Payload 是**纯出方向**的数据，字段只会被 master 读取和展示。
// 任何字段都不得被 master 用作指令、路径、命令或回调地址。
type Payload struct {
	NodeID       string            `json:"node_id"`
	Labels       map[string]string `json:"labels,omitempty"`
	TS           int64             `json:"ts"` // Unix 秒
	Seq          uint64            `json:"seq"`
	AgentVersion string            `json:"agent_version"`
	IntervalSec  int               `json:"interval_sec"`
	Host         HostInfo          `json:"host"`
	CPU          *CPUStat          `json:"cpu,omitempty"`
	Mem          *MemStat          `json:"mem,omitempty"`
	Disk         *DiskStat         `json:"disk,omitempty"`
	Net          *NetStat          `json:"net,omitempty"`
	GPU          []GPUStat         `json:"gpu,omitempty"`
	Temps        []TempStat        `json:"temps,omitempty"`
	// MaxTempC 是所有传感器里的最高温，用于卡片一眼看出"哪台机器烫"。
	MaxTempC  float64  `json:"max_temp_c,omitempty"`
	CollectMS int64    `json:"collect_ms"`
	Errors    []string `json:"errors,omitempty"`

	// Buffered / DroppedTotal 用于让主控识别「链路抖动导致的补发」。
	// Buffered 表示这条样本前面还压着多少条待补发的历史样本。
	Buffered     int    `json:"buffered,omitempty"`
	DroppedTotal uint64 `json:"dropped_total,omitempty"`
}

// HostInfo 描述节点静态/低频信息。
type HostInfo struct {
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Kernel    string `json:"kernel,omitempty"`
	Container string `json:"container,omitempty"` // docker / k8s / ""（裸机）
	UptimeSec uint64 `json:"uptime_sec"`
	BootTime  int64  `json:"boot_time,omitempty"`
}

// CPUStat 是 CPU 观测值。UsagePct 为两次采样之间的增量占用率。
//
// 核心数刻意拆成两个字段，因为它们是两个不同的量，混淆会差一倍：
//
//	Cores   物理核心（8 核 16 线程的机器上是 8）
//	Threads 逻辑处理器 / 线程（同一台机器上是 16）
//
// 识别不出物理核心时 Cores 保持 0，看板只显示线程数——
// 拿线程数冒充核心数比"不知道"更糟。
type CPUStat struct {
	UsagePct float64 `json:"usage_pct"`
	Cores    int     `json:"cores"`
	Threads  int     `json:"threads,omitempty"`
	// Sockets 是物理插槽（路）数，用来区分「8 核」是 1 路 ×8 还是 2 路 ×4。
	Sockets int       `json:"sockets,omitempty"`
	Load1   float64   `json:"load1,omitempty"`
	Load5   float64   `json:"load5,omitempty"`
	Load15  float64   `json:"load15,omitempty"`
	FreqMHz float64   `json:"freq_mhz,omitempty"`
	TempC   float64   `json:"temp_c,omitempty"`
	PerCore []float64 `json:"per_core,omitempty"`
}

// MemStat 是内存与 Swap 观测值，单位均为字节。
type MemStat struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPct        float64 `json:"used_pct"`
	SwapTotalBytes uint64  `json:"swap_total_bytes,omitempty"`
	SwapUsedBytes  uint64  `json:"swap_used_bytes,omitempty"`
}

// MountStat 是单个挂载点的磁盘观测值。
type MountStat struct {
	Mount      string  `json:"mount"`
	Device     string  `json:"device,omitempty"`
	FSType     string  `json:"fs,omitempty"`
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	AvailBytes uint64  `json:"avail_bytes"`
	UsedPct    float64 `json:"used_pct"`
	InodePct   float64 `json:"inode_pct,omitempty"`
}

// DiskStat 汇总所有被监控挂载点。
type DiskStat struct {
	Mounts     []MountStat `json:"mounts"`
	TotalBytes uint64      `json:"total_bytes"`
	UsedBytes  uint64      `json:"used_bytes"`
	UsedPct    float64     `json:"used_pct"`
	// MaxUsedPct 是单挂载点最高使用率，用于告警阈值判断（比总量平均更有意义）。
	MaxUsedPct float64 `json:"max_used_pct"`
}

// IfaceStat 是单网卡速率，单位为字节/秒。
type IfaceStat struct {
	Name       string  `json:"name"`
	RxBytesSec float64 `json:"rx_bytes_sec"`
	TxBytesSec float64 `json:"tx_bytes_sec"`
}

// NetStat 汇总网络吞吐。
type NetStat struct {
	RxBytesSec float64     `json:"rx_bytes_sec"`
	TxBytesSec float64     `json:"tx_bytes_sec"`
	Interfaces []IfaceStat `json:"interfaces,omitempty"`
}

// GPUStat 是单块 GPU 观测值。
type GPUStat struct {
	Index         int     `json:"index"`
	Name          string  `json:"name"`
	UUID          string  `json:"uuid,omitempty"`
	UtilPct       float64 `json:"util_pct"`
	MemTotalBytes uint64  `json:"mem_total_bytes"`
	MemUsedBytes  uint64  `json:"mem_used_bytes"`
	MemUsedPct    float64 `json:"mem_used_pct"`
	TempC         float64 `json:"temp_c,omitempty"`
	PowerW        float64 `json:"power_w,omitempty"`
	FanPct        float64 `json:"fan_pct,omitempty"`
}

// 温度传感器分类。看板按类别分组展示，历史曲线也按类别查询。
const (
	TempKindCPU   = "cpu"   // CPU 封装 / 核心
	TempKindGPU   = "gpu"   // 显卡
	TempKindDisk  = "disk"  // NVMe / SATA / RAID 硬盘
	TempKindBoard = "board" // 主板、机箱、进风口
	TempKindNIC   = "nic"   // 网卡光模块 / ASIC
	TempKindOther = "other" // 其他（电池、电池、ACPI 热区等）
)

// TempStat 是单个温度传感器读数。
//
// 设计说明：温度不是一个数，而是一组带名字的传感器。x86 服务器上
// 通常同时存在 CPU 封装、每个核心、主板、每块 NVMe、每个光模块的温度，
// 只上报一个"温度"会丢掉真正有用的信息（比如"三号盘的 NVMe 已经 78 度"）。
type TempStat struct {
	// Name 是可读名，形如 "CPU · Package id 0"、"硬盘 · Composite"。
	Name string `json:"name"`
	// Kind 用于分组与聚合，取值为上面 TempKind* 常量。
	Kind string `json:"kind"`
	// Source 是采集来源标识，形如 "hwmon:coretemp/temp1"，在同一台机器上稳定。
	Source string  `json:"source,omitempty"`
	TempC  float64 `json:"temp_c"`
	// MaxC / CritC 是内核/固件给出的阈值，用于让看板能算出"离过热还有多远"。
	MaxC  float64 `json:"max_c,omitempty"`
	CritC float64 `json:"crit_c,omitempty"`
}
