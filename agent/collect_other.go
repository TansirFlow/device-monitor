//go:build !linux && !windows

package main

import (
	"context"
	"os"
	"runtime"
	"time"
)

// 目前完整采集器覆盖 Linux（/proc、/sys）与 Windows（kernel32 只读 API）。
// 其他平台仍可编译运行并上报基础存活信息，方便先接入再补采集器。

type cpuTimes struct {
	idle  uint64
	total uint64
}

type netCounters struct {
	rx uint64
	tx uint64
}

type platformState struct {
	// 计数器而非 bool：main() 会先做一次预热采集，一次性标记会被它吃掉，
	// 导致这条提示永远到不了主控。
	notifyCount int
}

func newPlatformState() platformState { return platformState{} }

func collectPlatform(ctx context.Context, st *platformState, cfg *Config, errs *[]string) platformResult {
	host := &HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if name, err := os.Hostname(); err == nil {
		host.Hostname = name
	}
	const notifyTimes = 3
	if st.notifyCount < notifyTimes {
		*errs = append(*errs, "platform: 当前系统 "+runtime.GOOS+" 尚无完整采集器，仅上报主机存活信息")
		st.notifyCount++
	}
	_ = time.Now
	return platformResult{Host: host}
}
