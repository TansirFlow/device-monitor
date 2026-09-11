//go:build linux

package main

import (
	"bufio"
	"context"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 本文件只做一件事：读 /proc 与 /sys 里的文本/结构体。
// 不含任何 exec、不写文件、不监听端口。

type cpuTimes struct {
	idle  uint64
	total uint64
}

type netCounters struct {
	rx uint64
	tx uint64
}

type platformState struct {
	prevCPU    []cpuTimes
	hasPrevCPU bool
	prevNet    map[string]netCounters
	prevNetTS  time.Time
	hasPrevNet bool
}

func newPlatformState() platformState {
	return platformState{}
}

// pseudoFS 是无需监控的伪文件系统。
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"cgroup": true, "cgroup2": true, "securityfs": true, "pstore": true,
	"efivarfs": true, "bpf": true, "autofs": true, "mqueue": true,
	"hugetlbfs": true, "debugfs": true, "tracefs": true, "fusectl": true,
	"configfs": true, "ramfs": true, "nsfs": true, "binfmt_misc": true,
	"rpc_pipefs": true, "selinuxfs": true, "squashfs": true, "iso9660": true,
	"fuse.portal": true, "fuse.gvfsd-fuse": true, "fuse.lxcfs": true,
}

func collectPlatform(ctx context.Context, st *platformState, cfg *Config, errs *[]string) platformResult {
	var res platformResult
	res.Host = collectHost()

	if cfg.Collect.CPU {
		res.CPU = collectCPU(st, errs, cfg.Collect.PerCore)
	}
	if cfg.Collect.Mem {
		res.Mem = collectMem(errs)
	}
	if cfg.Collect.Disk {
		res.Disk = collectDisk(cfg, errs)
	}
	if cfg.Collect.Net {
		res.Net = collectNet(st, errs)
	}
	if cfg.Collect.Temp {
		res.Temps = collectTempsSysfs(errs)
	}
	return res
}

func collectHost() *HostInfo {
	h := &HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if name, err := os.Hostname(); err == nil {
		h.Hostname = name
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	if up, err := readUptime(); err == nil {
		h.UptimeSec = up
		h.BootTime = time.Now().Unix() - int64(up)
	}
	switch {
	case fileExists("/.dockerenv"):
		h.Container = "docker"
	case fileExistsRunKube():
		h.Container = "k8s"
	}
	return h
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileExistsRunKube() bool {
	// 只做只读探测，不执行任何外部命令。
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "kubepods")
}

func readUptime() (uint64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, nil
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return uint64(v), nil
}

// ---------- CPU ----------

// readCPUTimes 返回 [0]=总体, [1..N]=各核心。
func readCPUTimes() ([]cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []cpuTimes
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			break // cpu 行连续排在最前面，遇到别的行即可收工
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		var vals []uint64
		for _, fs := range fields[1:] {
			v, err := strconv.ParseUint(fs, 10, 64)
			if err != nil {
				break
			}
			vals = append(vals, v)
		}
		if len(vals) < 4 {
			continue
		}
		var total uint64
		for i, v := range vals {
			// guest / guest_nice 已被 user / nice 计入，跳过避免重复累加
			if i == 8 || i == 9 {
				continue
			}
			total += v
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4] // iowait 视为空闲
		}
		out = append(out, cpuTimes{idle: idle, total: total})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, os.ErrInvalid
	}
	return out, nil
}

func collectCPU(st *platformState, errs *[]string, perCore bool) *CPUStat {
	cur, err := readCPUTimes()
	if err != nil {
		*errs = append(*errs, "cpu: "+err.Error())
		return nil
	}
	cs := &CPUStat{}
	cs.setTopo(linuxCPUTopo(len(cur)))
	if st.hasPrevCPU && len(st.prevCPU) == len(cur) {
		cs.UsagePct = usageDelta(st.prevCPU[0], cur[0])
		if perCore {
			pc := make([]float64, 0, len(cur)-1)
			for i := 1; i < len(cur); i++ {
				pc = append(pc, usageDelta(st.prevCPU[i], cur[i]))
			}
			cs.PerCore = pc
		}
	}
	// 首次采样没有对比基线，只能报 0；下一个周期即恢复正常。
	st.prevCPU = cur
	st.hasPrevCPU = true

	cs.Load1, cs.Load5, cs.Load15 = readLoadAvg()
	cs.FreqMHz = readCPUFreqMHz()
	// CPU 温度不在这里取：统一由 collectTempsSysfs 的传感器列表回填（见 collect.go），
	// 避免"同一个 CPU 封装温度被两套逻辑各读一遍"。
	return cs
}

func readLoadAvg() (float64, float64, float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	c, _ := strconv.ParseFloat(f[1], 64)
	d, _ := strconv.ParseFloat(f[2], 64)
	return a, c, d
}

func readCPUFreqMHz() float64 {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	var sum float64
	var n int
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu MHz") {
			continue
		}
		_, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err == nil && f > 0 {
			sum += f
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Round(sum/float64(n)*10) / 10
}

// ---------- 温度 ----------
//
// 具体的 sysfs 解析规则（hwmon 芯片型号映射、thermal_zone 归类与去重、
// milli-degree 换算、trip_point critical 解析）都在 tempsysfs.go。
// 那个文件没有 build tag，因此可以在任意平台上用 fixture 目录做单元测试。

// ---------- 内存 ----------

func collectMem(errs *[]string) *MemStat {
	info, err := readMemInfo()
	if err != nil {
		*errs = append(*errs, "mem: "+err.Error())
		return nil
	}
	total := info["MemTotal"]
	if total == 0 {
		*errs = append(*errs, "mem: MemTotal 读取为空")
		return nil
	}
	avail := info["MemAvailable"]
	if avail == 0 {
		// 老内核没有 MemAvailable，用 Free+Buffers+Cached 近似
		avail = info["MemFree"] + info["Buffers"] + info["Cached"] + info["SReclaimable"]
	}
	used := uint64(0)
	if total > avail {
		used = total - avail
	}
	m := &MemStat{
		TotalBytes:     total,
		UsedBytes:      used,
		AvailableBytes: avail,
		UsedPct:        clampPct(float64(used) / float64(total) * 100),
	}
	m.SwapTotalBytes = info["SwapTotal"]
	if m.SwapTotalBytes > 0 {
		free := info["SwapFree"]
		if m.SwapTotalBytes > free {
			m.SwapUsedBytes = m.SwapTotalBytes - free
		}
	}
	return m
}

func readMemInfo() (map[string]uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]uint64, 32)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo 单位是 kB，统一转成字节
		out[k] = n * 1024
	}
	return out, sc.Err()
}

// ---------- 磁盘 ----------

func collectDisk(cfg *Config, errs *[]string) *DiskStat {
	mounts, err := listMounts()
	if err != nil {
		*errs = append(*errs, "disk: "+err.Error())
		return nil
	}
	allowed := map[string]bool{}
	for _, m := range cfg.Collect.DiskMounts {
		allowed[cleanMount(m)] = true
	}

	ds := &DiskStat{}
	seen := map[string]bool{}
	for _, mp := range mounts {
		if len(allowed) > 0 && !allowed[mp.mount] {
			continue
		}
		if seen[mp.mount] {
			continue
		}
		seen[mp.mount] = true

		total, used, avail, inodePct, err := statfsUsage(mp.mount)
		if err != nil || total == 0 {
			continue
		}
		ms := MountStat{
			Mount:      mp.mount,
			Device:     mp.device,
			FSType:     mp.fs,
			TotalBytes: total,
			UsedBytes:  used,
			AvailBytes: avail,
			InodePct:   inodePct,
		}
		if total+avail > 0 {
			ms.UsedPct = clampPct(float64(used) / float64(used+avail) * 100)
		}
		if ms.UsedPct > ds.MaxUsedPct {
			ds.MaxUsedPct = ms.UsedPct
		}
		ds.Mounts = append(ds.Mounts, ms)
		ds.TotalBytes += total
		ds.UsedBytes += used
	}
	if ds.TotalBytes > 0 {
		ds.UsedPct = clampPct(float64(ds.UsedBytes) / float64(ds.TotalBytes) * 100)
	}
	if len(ds.Mounts) == 0 {
		*errs = append(*errs, "disk: 未匹配到任何挂载点")
	}
	return ds
}

type rawMount struct {
	device string
	mount  string
	fs     string
}

// listMounts 读取 /proc/mounts 并过滤掉伪文件系统和重复项。
func listMounts() ([]rawMount, error) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil, err
	}
	var out []rawMount
	seenDev := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		fs := fields[2]
		if pseudoFS[fs] {
			continue
		}
		mnt := unescapeMount(fields[1])
		if strings.HasPrefix(mnt, "/sys") || strings.HasPrefix(mnt, "/proc") ||
			strings.HasPrefix(mnt, "/dev") || strings.HasPrefix(mnt, "/run") ||
			strings.HasPrefix(mnt, "/var/lib/docker/") ||
			strings.HasPrefix(mnt, "/var/lib/kubelet/") ||
			strings.HasPrefix(mnt, "/snap/") {
			continue
		}
		dev := fields[0]
		key := dev + "\x00" + mnt
		if seenDev[key] {
			continue
		}
		seenDev[key] = true
		out = append(out, rawMount{device: dev, mount: mnt, fs: fs})
	}
	return out, nil
}

// unescapeMount 处理 mountinfo 中的八进制转义（空格 \040 等）。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) || len(s) < 4 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func statfsUsage(path string) (total, used, avail uint64, inodePct float64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	free := st.Bfree * bs
	avail = st.Bavail * bs
	if total >= free {
		used = total - free
	}
	if st.Files > 0 {
		usedInodes := st.Files - st.Ffree
		inodePct = clampPct(float64(usedInodes) / float64(st.Files) * 100)
	}
	return
}

// ---------- 网络 ----------

func collectNet(st *platformState, errs *[]string) *NetStat {
	cur, err := readNetDev()
	if err != nil {
		*errs = append(*errs, "net: "+err.Error())
		return nil
	}
	now := time.Now()
	ns := &NetStat{}
	if st.hasPrevNet && len(st.prevNet) > 0 {
		elapsed := now.Sub(st.prevNetTS).Seconds()
		if elapsed <= 0 {
			elapsed = float64(time.Second)
		}
		for name, c := range cur {
			p, ok := st.prevNet[name]
			if !ok {
				continue
			}
			if c.rx < p.rx || c.tx < p.tx {
				continue // 计数器回绕或网卡重建，跳过本次
			}
			rx := float64(c.rx-p.rx) / elapsed
			tx := float64(c.tx-p.tx) / elapsed
			ns.RxBytesSec += rx
			ns.TxBytesSec += tx
			ns.Interfaces = append(ns.Interfaces, IfaceStat{Name: name, RxBytesSec: round1(rx), TxBytesSec: round1(tx)})
		}
	}
	ns.RxBytesSec = round1(ns.RxBytesSec)
	ns.TxBytesSec = round1(ns.TxBytesSec)
	st.prevNet = cur
	st.prevNetTS = now
	st.hasPrevNet = true
	return ns
}

func readNetDev() (map[string]netCounters, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]netCounters{}
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			continue // 表头
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[name] = netCounters{rx: rx, tx: tx}
	}
	return out, sc.Err()
}
