package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// collectGPU 通过 nvidia-smi 采集 NVIDIA GPU 指标。
//
// 安全说明（重要）：
//   - 这里调用的是**固定参数向量**，参数是编译期常量，不含任何来自主控、网络或
//     配置的外部输入（唯一可配的是二进制路径本身，属于本地配置）。
//     也就是说 master 无论如何都无法通过上报链路影响本进程执行什么命令。
//   - 调用带超时，失败不影响其他指标。
//   - 若节点没有 GPU，nvidia-smi 不存在时会静默跳过，不产生错误噪声。
func collectGPU(ctx context.Context, cfg *Config, errs *[]string) []GPUStat {
	if !cfg.Collect.GPU || !cfg.GPU.Enabled {
		return nil
	}
	bin := cfg.GPU.Binary
	if bin == "" {
		bin = "nvidia-smi"
	}

	const query = "index,name,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,fan.speed"
	args := []string{"--query-gpu=" + query, "--format=csv,noheader,nounits"}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		if isMissingBinary(err) {
			return nil // 无 GPU 或无驱动，属于正常情况
		}
		*errs = append(*errs, "gpu: 调用 nvidia-smi 失败: "+err.Error())
		return nil
	}

	var stats []GPUStat
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 8 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		g := GPUStat{
			Index:         int(parseNum(f[0])),
			Name:          f[1],
			UtilPct:       clampPct(parseNum(f[3])),
			MemUsedBytes:  mibToBytes(parseNum(f[4])),
			MemTotalBytes: mibToBytes(parseNum(f[5])),
			TempC:         round1(parseNum(f[6])),
			PowerW:        round1(parseNum(f[7])),
		}
		if cfg.GPU.IncludeUUID && len(f) > 2 {
			g.UUID = f[2]
		}
		if len(f) > 8 {
			g.FanPct = clampPct(parseNum(f[8]))
		}
		if g.MemTotalBytes > 0 {
			g.MemUsedPct = clampPct(float64(g.MemUsedBytes) / float64(g.MemTotalBytes) * 100)
		}
		stats = append(stats, g)
	}
	return stats
}

func isMissingBinary(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*exec.Error); ok {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found") ||
		strings.Contains(err.Error(), "cannot find the file")
}

func mibToBytes(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v * 1024 * 1024)
}

// parseNum 解析 nvidia-smi 的数值字段，兼容 "[N/A]" / "N/A" / "[Not Supported]"。
func parseNum(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "[]")
	if s == "" || strings.EqualFold(s, "N/A") || strings.Contains(strings.ToLower(s), "not supported") {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}
