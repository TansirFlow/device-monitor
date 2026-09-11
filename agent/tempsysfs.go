package main

// sysfs 温度解析器（Linux）。
//
// 本文件**故意不加 //go:build linux**：
// 这里全部是「在一棵目录树上按约定文件名读文本」的纯逻辑，
// 与操作系统无关，只有挂载前缀不同。如果把它塞进带 linux tag 的文件里，
// 这些解析规则就永远无法被测试（CI 与大多数开发机是 Windows/macOS），
// 而它恰恰是这套采集器里最容易写错、又最难在真实硬件上复现的部分。
//
// 在非 Linux 平台，这些函数不会被任何代码引用，会被链接器整个丢弃，
// 不会进入最终二进制。
//
// Linux 上温度来自两个只读 sysfs 目录，不需要外部命令、不需要 root：
//
//	/sys/class/hwmon/hwmon*    主来源。带芯片名、通道标签以及 max/crit 阈值
//	/sys/class/thermal/*       补充来源。ARM 板卡（树莓派等）通常只有这个

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sysRoot / sysfsPath / readTrimmed 这些公共设施在 sysfs.go 里定义，
// 与 CPU 拓扑解析共用同一套可注入的根路径。

// hwmonChipKind 把 hwmon 芯片名映射到温度类别，未列出的归入 other。
var hwmonChipKind = map[string]string{
	// CPU
	"coretemp": TempKindCPU, "k10temp": TempKindCPU, "zenpower": TempKindCPU,
	"k8temp": TempKindCPU, "cpu_thermal": TempKindCPU, "soc_thermal": TempKindCPU,
	// 显卡
	"amdgpu": TempKindGPU, "nouveau": TempKindGPU, "radeon": TempKindGPU,
	"i915": TempKindGPU, "xe": TempKindGPU,
	// 硬盘
	"nvme": TempKindDisk, "drivetemp": TempKindDisk,
	// 主板 / 机箱 Super I/O
	"acpitz": TempKindBoard, "nct6775": TempKindBoard, "nct6776": TempKindBoard,
	"nct6779": TempKindBoard, "nct6791": TempKindBoard, "nct6792": TempKindBoard,
	"nct6793": TempKindBoard, "nct6795": TempKindBoard, "nct6796": TempKindBoard,
	"nct6797": TempKindBoard, "nct6798": TempKindBoard, "nct6683": TempKindBoard,
	"nct6687": TempKindBoard, "it87": TempKindBoard, "it8728": TempKindBoard,
	"it8688": TempKindBoard, "f71882fg": TempKindBoard, "w83627ehf": TempKindBoard,
	"w83627dhg": TempKindBoard, "dell_smm": TempKindBoard, "asus": TempKindBoard,
	"asus_wmi_sensors": TempKindBoard, "thinkpad": TempKindBoard,
	"ibmpex": TempKindBoard, "applesmc": TempKindBoard,
	// 网卡 / 光模块
	"mlx5": TempKindNIC, "i40e": TempKindNIC, "ixgbe": TempKindNIC,
	"ice": TempKindNIC, "igb": TempKindNIC, "bnxt": TempKindNIC,
	"qede": TempKindNIC, "atlantic": TempKindNIC, "sfc": TempKindNIC,
}

var tempKindLabel = map[string]string{
	TempKindCPU:   "CPU",
	TempKindGPU:   "GPU",
	TempKindDisk:  "硬盘",
	TempKindBoard: "主板",
	TempKindNIC:   "网卡",
	TempKindOther: "其他",
}

// collectTempsSysfs 汇总本机 sysfs 温度传感器。
//
// 策略：hwmon 优先，thermal_zone 只补 hwmon 没覆盖到的类别。
// 这样 x86 上不会出现「coretemp 的 Package 与 thermal_zone 的 x86_pkg_temp
// 各报一遍」的重复，ARM 上也不会因为缺 hwmon 而丢掉 CPU 温度。
func collectTempsSysfs(errs *[]string) []TempStat {
	sensors := readHwmonSensors()

	covered := map[string]bool{}
	for _, s := range sensors {
		covered[s.Kind] = true
	}
	for _, s := range readThermalZones() {
		if covered[s.Kind] {
			continue // 同一类别已有更详细的数据源
		}
		sensors = append(sensors, s)
	}

	if len(sensors) == 0 {
		*errs = append(*errs, "temp: 未在 /sys/class/{hwmon,thermal} 读到温度传感器"+
			"（容器内通常需要挂载 /sys，且需要能访问宿主机的 hwmon）")
	}
	return sensors
}

// readHwmonSensors 遍历所有 hwmon 芯片的 tempN_input 通道。
func readHwmonSensors() []TempStat {
	dirs, err := filepath.Glob(sysfsPath("class", "hwmon", "hwmon*"))
	if err != nil {
		return nil
	}
	var out []TempStat
	for _, dir := range dirs {
		chip := readTrimmed(filepath.Join(dir, "name"))
		if chip == "" {
			chip = filepath.Base(dir) // 极少数芯片没有 name 文件
		}
		kind := hwmonChipKind[chip]
		if kind == "" {
			kind = TempKindOther
		}
		label := tempKindLabel[kind]

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			idx, ok := tempInputIndex(e.Name())
			if !ok {
				continue
			}
			c := readTempMilli(filepath.Join(dir, e.Name()))
			if c <= 0 {
				continue
			}
			display := readTrimmed(filepath.Join(dir, fmt.Sprintf("temp%d_label", idx)))
			if display == "" {
				display = fmt.Sprintf("传感器 %d", idx)
			}
			out = append(out, TempStat{
				Name:   label + " · " + display,
				Kind:   kind,
				Source: fmt.Sprintf("hwmon:%s/temp%d", chip, idx),
				TempC:  c,
				MaxC:   readTempMilli(filepath.Join(dir, fmt.Sprintf("temp%d_max", idx))),
				CritC:  readTempMilli(filepath.Join(dir, fmt.Sprintf("temp%d_crit", idx))),
			})
		}
	}
	return out
}

// readThermalZones 读取 ACPI / SoC 热区。ARM 平台的主力来源。
func readThermalZones() []TempStat {
	zones, err := filepath.Glob(sysfsPath("class", "thermal", "thermal_zone*"))
	if err != nil {
		return nil
	}
	var out []TempStat
	for _, z := range zones {
		c := readTempMilli(filepath.Join(z, "temp"))
		if c <= 0 {
			continue
		}
		typ := readTrimmed(filepath.Join(z, "type"))
		kind := thermalTypeKind(typ)
		if typ == "" {
			typ = filepath.Base(z)
		}
		out = append(out, TempStat{
			Name:   tempKindLabel[kind] + " · " + typ,
			Kind:   kind,
			Source: "thermal:" + filepath.Base(z),
			TempC:  c,
			CritC:  thermalCritTemp(z),
		})
	}
	return out
}

func thermalTypeKind(typ string) string {
	t := strings.ToLower(typ)
	switch {
	case strings.Contains(t, "x86_pkg_temp"), strings.Contains(t, "cpu"),
		strings.Contains(t, "soc"), strings.Contains(t, "cluster"):
		return TempKindCPU
	case strings.Contains(t, "gpu"), strings.Contains(t, "amdgpu"), strings.Contains(t, "nouveau"):
		return TempKindGPU
	case strings.Contains(t, "nvme"), strings.Contains(t, "ssd"):
		return TempKindDisk
	case strings.Contains(t, "acpitz"), strings.Contains(t, "board"), strings.Contains(t, "pch"):
		return TempKindBoard
	}
	return TempKindOther
}

// thermalCritTemp 找出该热区的 critical 跳变点，作为过热阈值。
func thermalCritTemp(zone string) float64 {
	for i := 0; i < 10; i++ {
		typ := readTrimmed(filepath.Join(zone, fmt.Sprintf("trip_point_%d_type", i)))
		if typ == "" {
			break
		}
		if strings.EqualFold(typ, "critical") {
			return readTempMilli(filepath.Join(zone, fmt.Sprintf("trip_point_%d_temp", i)))
		}
	}
	return 0
}

// tempInputIndex 从 "temp3_input" 解析出通道号；不匹配返回 false。
func tempInputIndex(name string) (int, bool) {
	name = strings.ToLower(name)
	if !strings.HasPrefix(name, "temp") || !strings.HasSuffix(name, "_input") {
		return 0, false
	}
	mid := name[4 : len(name)-len("_input")]
	n, err := strconv.Atoi(mid)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// readTempMilli 读取毫摄氏度文件并转成摄氏度（保留 1 位小数）。读不到返回 0。
func readTempMilli(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return math.Round(v/100.0) / 10
}
