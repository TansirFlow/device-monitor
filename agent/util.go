package main

import "math"

// usageDelta 计算两次 CPU 时间快照之间的占用率。
// cpuTimes 由各平台的采集文件定义，因此这个函数可以跨平台复用。
func usageDelta(prev, cur cpuTimes) float64 {
	dt := float64(cur.total) - float64(prev.total)
	if dt <= 0 {
		return 0
	}
	di := float64(cur.idle) - float64(prev.idle)
	return clampPct((1 - di/dt) * 100)
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return round2(v)
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func round1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}
