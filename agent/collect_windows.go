//go:build windows

package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type cpuTimes struct {
	idle  uint64
	total uint64
}

type netCounters struct {
	rx uint64
	tx uint64
}

type platformState struct {
	prevCPU    cpuTimes
	hasPrevCPU bool
	// netWarnCount 让「Windows 暂无网络采集」这条提示在前几次真实上报里都出现。
	// 用计数器而不是 bool 是因为 main() 会先做一次预热采集，
	// 一次性的标记会被预热吃掉，用户反而永远看不到这条提示。
	netWarnCount int
	prevNetTS    time.Time
}

func newPlatformState() platformState { return platformState{} }

var (
	ntdll                   = syscall.NewLazyDLL("ntdll.dll")
	procRtlGetVersion       = ntdll.NewProc("RtlGetVersion")
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes      = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatus  = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64      = kernel32.NewProc("GetTickCount64")
	procGetLogicalDrivesStr = kernel32.NewProc("GetLogicalDriveStringsW")
	procGetDriveType        = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceEx  = kernel32.NewProc("GetDiskFreeSpaceExW")
)

const driveFixed = 3

type filetime struct {
	low  uint32
	high uint32
}

func ftToU64(ft filetime) uint64 { return uint64(ft.high)<<32 | uint64(ft.low) }

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// 本文件只调用 kernel32 的只读查询 API，不执行任何外部程序、不写文件、不开端口。

func collectPlatform(ctx context.Context, st *platformState, cfg *Config, errs *[]string) platformResult {
	var res platformResult

	host := &HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if name, err := os.Hostname(); err == nil {
		host.Hostname = name
	}
	if b, err := readKernelBuild(); err == nil {
		host.Kernel = b
	}
	if ms, err := uptimeMillis(); err == nil {
		host.UptimeSec = ms / 1000
		host.BootTime = time.Now().Unix() - int64(ms/1000)
	}
	res.Host = host

	if cfg.Collect.CPU {
		res.CPU = collectCPUWindows(st, errs)
	}
	if cfg.Collect.Mem {
		res.Mem = collectMemWindows(errs)
	}
	if cfg.Collect.Disk {
		res.Disk = collectDiskWindows(cfg, errs)
	}
	if cfg.Collect.Temp {
		res.Temps = collectTempsWindows(cfg)
	}
	const netWarnTimes = 3
	if cfg.Collect.Net && st.netWarnCount < netWarnTimes {
		*errs = append(*errs, "net: Windows 平台暂未实现网络采集（不影响其他指标）")
		st.netWarnCount++
	}
	return res
}

func uptimeMillis() (uint64, error) {
	r, _, err := procGetTickCount64.Call()
	if r == 0 && err != nil && err != syscall.Errno(0) {
		return 0, err
	}
	return uint64(r), nil
}

// osVersionInfoExW 对应 Windows 的 RTL_OSVERSIONINFOEXW。
// 用 RtlGetVersion 而不是已废弃的 GetVersionEx——后者在 Win8.1+ 上会撒谎。
type osVersionInfoExW struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
	ServicePackMajor  uint16
	ServicePackMinor  uint16
	SuiteMask         uint16
	ProductType       byte
	Reserved          byte
}

func readKernelBuild() (string, error) {
	var vi osVersionInfoExW
	vi.OSVersionInfoSize = uint32(unsafe.Sizeof(vi))
	r, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&vi)))
	if r != 0 { // NTSTATUS 非 0 表示失败
		return "", &syscallError{op: "RtlGetVersion"}
	}
	return strconv.FormatUint(uint64(vi.MajorVersion), 10) + "." +
		strconv.FormatUint(uint64(vi.MinorVersion), 10) + "." +
		strconv.FormatUint(uint64(vi.BuildNumber), 10), nil
}

func collectCPUWindows(st *platformState, errs *[]string) *CPUStat {
	var idle, kern, user filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kern)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		*errs = append(*errs, "cpu: GetSystemTimes 调用失败: "+errString(err))
		return nil
	}
	// GetSystemTimes 的 kernel 时间已包含 idle，因此 total = kernel + user。
	cur := cpuTimes{
		idle:  ftToU64(idle),
		total: ftToU64(kern) + ftToU64(user),
	}
	// 注意：不要用 runtime.NumCPU() 当核心数——那是逻辑处理器数。
	// 物理核心数由 GetLogicalProcessorInformationEx 给出（见 cputopo_windows.go）。
	cs := &CPUStat{}
	cs.setTopo(cpuTopoWindows())
	if st.hasPrevCPU {
		cs.UsagePct = usageDelta(st.prevCPU, cur)
	}
	st.prevCPU = cur
	st.hasPrevCPU = true
	return cs
}

func collectMemWindows(errs *[]string) *MemStat {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, err := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		*errs = append(*errs, "mem: GlobalMemoryStatusEx 调用失败: "+errString(err))
		return nil
	}
	used := uint64(0)
	if ms.TotalPhys > ms.AvailPhys {
		used = ms.TotalPhys - ms.AvailPhys
	}
	m := &MemStat{
		TotalBytes:     ms.TotalPhys,
		UsedBytes:      used,
		AvailableBytes: ms.AvailPhys,
		UsedPct:        clampPct(float64(ms.MemoryLoad)),
	}
	// Windows 的 TotalPageFile 是「提交上限」（物理内存 + 页面文件），
	// 减掉物理内存即为页面文件可用量，属于近似值。
	if ms.TotalPageFile > ms.TotalPhys {
		m.SwapTotalBytes = ms.TotalPageFile - ms.TotalPhys
		if ms.AvailPageFile > ms.AvailPhys {
			m.SwapUsedBytes = ms.AvailPageFile - ms.AvailPhys
			if m.SwapUsedBytes > m.SwapTotalBytes {
				m.SwapUsedBytes = m.SwapTotalBytes
			}
		}
	}
	return m
}

func collectDiskWindows(cfg *Config, errs *[]string) *DiskStat {
	var roots []string
	if len(cfg.Collect.DiskMounts) > 0 {
		roots = cfg.Collect.DiskMounts
	} else {
		var err error
		roots, err = listFixedDrives()
		if err != nil {
			*errs = append(*errs, "disk: 枚举驱动器失败: "+err.Error())
			return nil
		}
	}

	ds := &DiskStat{}
	for _, root := range roots {
		mp := cleanMount(root)
		total, used, avail, err := diskFreeSpace(root)
		if err != nil || total == 0 {
			continue
		}
		ms := MountStat{Mount: mp, FSType: "windows", TotalBytes: total, UsedBytes: used, AvailBytes: avail}
		if used+avail > 0 {
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
	return ds
}

func listFixedDrives() ([]string, error) {
	buf := make([]uint16, 512)
	r, _, err := procGetLogicalDrivesStr.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return nil, errnoOr(err, "GetLogicalDriveStringsW")
	}
	all := syscall.UTF16ToString(buf)
	var out []string
	for _, part := range strings.Split(all, "\x00") {
		if part == "" {
			continue
		}
		p, err := syscall.UTF16PtrFromString(part)
		if err != nil {
			continue
		}
		t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		if uint32(t) == driveFixed {
			out = append(out, part)
		}
	}
	return out, nil
}

func diskFreeSpace(root string) (total, used, avail uint64, err error) {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return
	}
	var freeCaller, totalBytes, totalFree uint64
	r, _, callErr := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		err = errnoOr(callErr, "GetDiskFreeSpaceExW")
		return
	}
	total = totalBytes
	avail = totalFree
	if total >= totalFree {
		used = total - totalFree
	}
	return
}

// ---------- 温度（Windows）----------
//
// Windows 没有 Linux 那样的 hwmon，能拿到什么完全取决于硬件与权限：
//
//   硬盘温度   IOCTL_STORAGE_QUERY_PROPERTY(StorageDeviceTemperatureProperty)
//              走 \\.\PhysicalDriveN，需要管理员权限才能拿到真实读数；
//              非管理员时驱动会返回一个空描述符，这里识别为"无数据"并静默跳过。
//   GPU 温度   由 gpu.go 的 nvidia-smi 提供（不依赖管理员）。
//   CPU 温度   没有公开的通用接口，必须装厂商驱动/软件（如 LibreHardwareMonitor）。
//              本项目不为此引入外部依赖或额外 exec，因此 Windows 上采不到 CPU 温度。
//
// 所有失败路径都不写 errs：Windows 上"采不到温度"是环境限制而非异常，
// 反复上报降级信息只会淹没真正的问题。

const (
	driveOpenShare    = 0x00000003 // FILE_SHARE_READ | FILE_SHARE_WRITE
	driveOpenExisting = 3          // OPEN_EXISTING

	ioctlStorageQueryProperty        = 0x2D1400
	storageDeviceProperty            = 0x00
	storageDeviceTemperatureProperty = 0x30
	propertyStandardQuery            = 0
)

// storagePropertyQuery 对应 STORAGE_PROPERTY_QUERY（sizeof = 12）。
type storagePropertyQuery struct {
	PropertyID           uint32
	QueryType            uint32
	AdditionalParameters [1]byte
}

var (
	invalidHandle       = ^uintptr(0)
	procCreateFileW     = kernel32.NewProc("CreateFileW")
	procDeviceIoControl = kernel32.NewProc("DeviceIoControl")
	procCloseHandle     = kernel32.NewProc("CloseHandle")
)

func collectTempsWindows(cfg *Config) []TempStat {
	var out []TempStat
	misses := 0
	for i := 0; i < 16; i++ {
		h, ok := openPhysicalDrive(i)
		if !ok {
			// 盘序号通常连续；连续多次打不开就认为枚举结束
			misses++
			if misses >= 3 {
				break
			}
			continue
		}
		misses = 0

		model := strings.TrimSpace(devicePropertyString(h, 16, 4096))
		if model == "" {
			model = fmt.Sprintf("PhysicalDrive%d", i)
		}
		if c, crit, ok := deviceTemperature(h); ok {
			name := model
			out = append(out, TempStat{
				Name:   "硬盘 · " + name,
				Kind:   TempKindDisk,
				Source: fmt.Sprintf("windows:physicaldrive%d", i),
				TempC:  c,
				CritC:  crit,
			})
		}
		procCloseHandle.Call(uintptr(h))
	}
	return out
}

// openPhysicalDrive 先尝试带读权限的句柄（温度属性需要），
// 失败则退回零权限句柄（型号属性可用）——两者都不需要写权限。
func openPhysicalDrive(i int) (syscall.Handle, bool) {
	path := `\\.\PhysicalDrive` + strconv.Itoa(i)
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	for _, access := range []uintptr{0x80000000 /* GENERIC_READ */, 0} {
		h, _, _ := procCreateFileW.Call(
			uintptr(unsafe.Pointer(p)), access, driveOpenShare, 0, driveOpenExisting, 0, 0)
		if h != invalidHandle {
			return syscall.Handle(h), true
		}
	}
	return 0, false
}

func deviceIoControl(h syscall.Handle, propID uint32, buf []byte) (uint32, bool) {
	if len(buf) == 0 {
		return 0, false
	}
	q := storagePropertyQuery{PropertyID: propID, QueryType: propertyStandardQuery}
	var ret uint32
	r, _, _ := procDeviceIoControl.Call(
		uintptr(h), ioctlStorageQueryProperty,
		uintptr(unsafe.Pointer(&q)), unsafe.Sizeof(q),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&ret)), 0,
	)
	if r == 0 || ret == 0 {
		return 0, false
	}
	return ret, true
}

// devicePropertyString 读取 STORAGE_DEVICE_DESCRIPTOR 里的一个偏移字段（型号/厂商等）。
func devicePropertyString(h syscall.Handle, offsetField int, bufSize int) string {
	buf := make([]byte, bufSize)
	ret, ok := deviceIoControl(h, storageDeviceProperty, buf)
	if !ok || ret < 36 || offsetField+4 > int(ret) {
		return ""
	}
	off := le32(buf[offsetField : offsetField+4])
	if off == 0 || off >= ret {
		return ""
	}
	end := int(off)
	for end < int(ret) && buf[end] != 0 {
		end++
	}
	return string(buf[int(off):end])
}

// deviceTemperature 解析 STORAGE_TEMPERATURE_DATA_DESCRIPTOR。
//
// 返回 (摄氏度, 过温阈值, 是否有效)。只有描述符里确实带了至少一个
// 温度条目才会返回值——空描述符（未提权时的典型表现）一律当作无数据。
func deviceTemperature(h syscall.Handle) (float64, float64, bool) {
	buf := make([]byte, 1024)
	ret, ok := deviceIoControl(h, storageDeviceTemperatureProperty, buf)
	if !ok || ret < 40 {
		return 0, 0, false
	}
	size := le32(buf[4:8])
	// Size 是描述符总长度（头部 24 字节 + 每个条目 16 字节）
	if size < 40 || size > ret || size > uint32(len(buf)) {
		return 0, 0, false
	}
	if (size-24)%16 != 0 {
		return 0, 0, false
	}

	// STORAGE_TEMPERATURE_INFO[0].Temperature（偏移 26，有符号 16 位）
	raw := float64(int16(le16(buf[26:28])))
	c, valid := normalizeTempC(raw)
	if !valid {
		return 0, 0, false
	}

	// CriticalTemperature 在偏移 10，单位摄氏度
	crit := float64(buf[10])
	if crit < 20 || crit > 125 || crit <= c {
		crit = 0
	}
	return c, crit, true
}

// normalizeTempC 处理温度的两种单位。
//
// 官方文档标注为开尔文，但确实存在直接返回摄氏度的驱动/固件。
// 两者的数值区间完全不重叠（摄氏 1~125，开尔文 274~398），
// 因此按区间自动判别是安全的，不需要假设。
func normalizeTempC(v float64) (float64, bool) {
	if v > 200 {
		v -= 273.15 // 开尔文 → 摄氏度
	}
	if v < 1 || v > 125 {
		return 0, false
	}
	return math.Round(v*10) / 10, true
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func errnoOr(err error, op string) error {
	if err == nil || err == syscall.Errno(0) {
		return &syscallError{op: op}
	}
	return err
}

type syscallError struct{ op string }

func (e *syscallError) Error() string { return e.op + " 调用失败" }
