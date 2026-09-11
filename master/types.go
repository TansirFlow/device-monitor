package main

// Sample 是 master 侧对 agent 上报数据的表示。
//
// 安全约定：master 把 Sample 当作**不可信的只读数据**。
// 所有字段只用于展示，绝不用于构造文件路径之外的行为——
// 而文件路径部分（node_id）在下游还会被正则白名单二次校验。
type Sample struct {
	NodeID       string            `json:"node_id"`
	Labels       map[string]string `json:"labels,omitempty"`
	TS           int64             `json:"ts"`
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
	MaxTempC     float64           `json:"max_temp_c,omitempty"`
	CollectMS    int64             `json:"collect_ms"`
	Errors       []string          `json:"errors,omitempty"`
	Buffered     int               `json:"buffered,omitempty"`
	DroppedTotal uint64            `json:"dropped_total,omitempty"`
}

// TempStat 是一个温度传感器读数。kind 用来归类（cpu/gpu/disk/board/nic/other），
// 看板按 kind 做聚合与配色；name 是传感器原始名字，只用于展示。
type TempStat struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Source string  `json:"source,omitempty"`
	TempC  float64 `json:"temp_c"`
	MaxC   float64 `json:"max_c,omitempty"`
	CritC  float64 `json:"crit_c,omitempty"`
}

type HostInfo struct {
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Kernel    string `json:"kernel,omitempty"`
	Container string `json:"container,omitempty"`
	UptimeSec uint64 `json:"uptime_sec"`
	BootTime  int64  `json:"boot_time,omitempty"`
}

// CPUStat 里的 Cores 是**物理核心**数，Threads 是**逻辑处理器**（线程）数。
// 两者必须分开显示：8 核 16 线程的机器上 Cores=8、Threads=16。
// Cores 为 0 表示 agent 未能识别物理核心（此时只展示 Threads）。
type CPUStat struct {
	UsagePct float64   `json:"usage_pct"`
	Cores    int       `json:"cores"`
	Threads  int       `json:"threads,omitempty"`
	Sockets  int       `json:"sockets,omitempty"`
	Load1    float64   `json:"load1,omitempty"`
	Load5    float64   `json:"load5,omitempty"`
	Load15   float64   `json:"load15,omitempty"`
	FreqMHz  float64   `json:"freq_mhz,omitempty"`
	TempC    float64   `json:"temp_c,omitempty"`
	PerCore  []float64 `json:"per_core,omitempty"`
}

type MemStat struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPct        float64 `json:"used_pct"`
	SwapTotalBytes uint64  `json:"swap_total_bytes,omitempty"`
	SwapUsedBytes  uint64  `json:"swap_used_bytes,omitempty"`
}

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

type DiskStat struct {
	Mounts     []MountStat `json:"mounts"`
	TotalBytes uint64      `json:"total_bytes"`
	UsedBytes  uint64      `json:"used_bytes"`
	UsedPct    float64     `json:"used_pct"`
	MaxUsedPct float64     `json:"max_used_pct"`
}

type IfaceStat struct {
	Name       string  `json:"name"`
	RxBytesSec float64 `json:"rx_bytes_sec"`
	TxBytesSec float64 `json:"tx_bytes_sec"`
}

type NetStat struct {
	RxBytesSec float64     `json:"rx_bytes_sec"`
	TxBytesSec float64     `json:"tx_bytes_sec"`
	Interfaces []IfaceStat `json:"interfaces,omitempty"`
}

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

// ---------- 面向看板的视图结构（只读） ----------

type NodeView struct {
	Sample
	Online        bool         `json:"online"`
	AgeSec        int64        `json:"age_sec"`
	StaleAfterSec int64        `json:"stale_after_sec"`
	Temp          *TempSummary `json:"temp,omitempty"`
}

type Point struct {
	TS int64   `json:"ts"`
	V  float64 `json:"v"`
}

type HistoryResp struct {
	Node   string  `json:"node"`
	Metric string  `json:"metric"`
	Unit   string  `json:"unit"`
	Step   int64   `json:"step"`
	From   int64   `json:"from"`
	To     int64   `json:"to"`
	Points []Point `json:"points"`
}
