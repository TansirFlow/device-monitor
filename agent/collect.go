package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// platformResult 是各平台采集器的统一返回值。
//
// 用结构体而不是多返回值，是因为指标会持续增加（温度就是这么加进来的），
// 多返回值的签名每加一项都要改三处调用点。
type platformResult struct {
	Host  *HostInfo
	CPU   *CPUStat
	Mem   *MemStat
	Disk  *DiskStat
	Net   *NetStat
	Temps []TempStat
}

// Collector 持有跨采样周期的状态（CPU/网络的增量计算需要上一次的快照）。
//
// 注意：这里的状态全部存在于内存，agent 运行期**不写任何文件**，
// 因此不存在被诱导落盘可执行内容的通道。
type Collector struct {
	cfg *Config
	seq uint64
	st  platformState
}

func newCollector(cfg *Config) *Collector {
	return &Collector{cfg: cfg, st: newPlatformState()}
}

// Collect 采集一次完整快照。任何一个子采集器失败都不会中断整体：
// 失败信息进 payload.errors，master 侧可据此展示「采集降级」。
func (c *Collector) Collect(ctx context.Context) *Payload {
	start := time.Now()
	c.seq++

	p := &Payload{
		NodeID:       c.cfg.NodeID,
		Labels:       c.cfg.Labels,
		TS:           start.Unix(),
		Seq:          c.seq,
		AgentVersion: version,
		IntervalSec:  c.cfg.IntervalSec,
	}

	var errs []string

	res := collectPlatform(ctx, &c.st, c.cfg, &errs)
	if res.Host != nil {
		p.Host = *res.Host
	}
	p.CPU = res.CPU
	p.Mem = res.Mem
	p.Disk = res.Disk
	p.Net = res.Net
	p.GPU = collectGPU(ctx, c.cfg, &errs)

	// 温度统一成一张表：平台采集器给的传感器 + GPU 温度。
	// Linux 上 NVIDIA 通常不注册 hwmon，GPU 温度只能来自 nvidia-smi，
	// 这里合并后，看板与历史查询就不用区分来源了。
	temps := res.Temps
	if c.cfg.Collect.Temp {
		for _, g := range p.GPU {
			if g.TempC <= 0 {
				continue
			}
			temps = append(temps, TempStat{
				Name:   fmt.Sprintf("GPU%d · %s", g.Index, shortGPUName(g.Name)),
				Kind:   TempKindGPU,
				Source: fmt.Sprintf("nvidia-smi:gpu%d", g.Index),
				TempC:  g.TempC,
			})
		}
	}
	p.Temps = finalizeTemps(temps, c.cfg)
	p.MaxTempC = maxTempOfKind(p.Temps, "")
	if p.CPU != nil && p.CPU.TempC == 0 {
		p.CPU.TempC = maxTempOfKind(p.Temps, TempKindCPU)
	}

	p.CollectMS = time.Since(start).Milliseconds()
	sort.Strings(errs)
	p.Errors = errs

	return p
}

// tempKindRank 决定温度传感器的保留优先级：机器传感器很多时先保重要的。
var tempKindRank = map[string]int{
	TempKindCPU:   0,
	TempKindGPU:   1,
	TempKindDisk:  2,
	TempKindBoard: 3,
	TempKindNIC:   4,
	TempKindOther: 5,
}

// finalizeTemps 过滤异常读数、应用排除规则、排序并按上限截断。
// 排序在类别内按名称，保证同一台机器的传感器顺序在多次采样间稳定。
func finalizeTemps(in []TempStat, cfg *Config) []TempStat {
	if len(in) == 0 {
		return nil
	}
	out := make([]TempStat, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, t := range in {
		// 0 表示读不到；>150 基本是寄存器解析错误（部分主板会返回 255）
		if t.TempC <= 0 || t.TempC > 150 {
			continue
		}
		if t.Kind == "" {
			t.Kind = TempKindOther
		}
		if excludedTemp(t, cfg.Collect.TempExclude) {
			continue
		}
		key := t.Source
		if key == "" {
			key = t.Kind + "\x00" + t.Name
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}

	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := tempKindRank[out[i].Kind], tempKindRank[out[j].Kind]
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	if lim := cfg.Collect.TempLimit; lim > 0 && len(out) > lim {
		out = out[:lim]
	}
	return out
}

func excludedTemp(t TempStat, patterns []string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.Contains(t.Name, p) || strings.Contains(t.Source, p) {
			return true
		}
	}
	return false
}

// maxTempOfKind 取某类别的最高温；kind 为空时取全部。
func maxTempOfKind(temps []TempStat, kind string) float64 {
	var best float64
	for _, t := range temps {
		if kind != "" && t.Kind != kind {
			continue
		}
		if t.TempC > best {
			best = t.TempC
		}
	}
	return best
}

// shortGPUName 去掉冗长的厂商前缀，让温度名保持一行可读。
func shortGPUName(name string) string {
	r := strings.NewReplacer(
		"NVIDIA GeForce ", "", "NVIDIA ", "",
		"AMD Radeon ", "", "Advanced Micro Devices, Inc. ", "",
	)
	return strings.TrimSpace(r.Replace(name))
}

// cleanMount 规范化挂载点路径，避免路径分隔符混用造成同一挂载点重复统计。
func cleanMount(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/\\")
	}
	if p == "" {
		return "/"
	}
	return filepath.ToSlash(p)
}
