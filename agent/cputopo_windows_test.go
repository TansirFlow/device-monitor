//go:build windows

package main

// Windows 侧的 CPU 拓扑测试。
//
// 前一半在**真实机器**上核对：物理核心数必须来自内核，且与 runtime.NumCPU()
// 不是一回事——8 核 16 线程的机器上 NumCPU()=16、物理核=8，
// 把前者当核心数就是当初那个 bug。后一半用合成记录验证处理器组解析。

import (
	"encoding/binary"
	"runtime"
	"testing"
)

func TestCPUTopoWindowsRealMachine(t *testing.T) {
	topo := cpuTopoWindows()

	if topo.Threads <= 0 {
		t.Fatalf("逻辑线程数应至少为 1，得到 %d", topo.Threads)
	}
	if topo.Cores <= 0 {
		t.Fatalf("GetLogicalProcessorInformationEx 未返回物理核心数（Cores=%d）", topo.Cores)
	}
	if topo.Cores > topo.Threads {
		t.Fatalf("物理核心 %d 不可能多于逻辑线程 %d", topo.Cores, topo.Threads)
	}
	if topo.Sockets <= 0 {
		t.Errorf("插槽数应至少为 1，得到 %d", topo.Sockets)
	}
	if topo.Threads != runtime.NumCPU() {
		// 逻辑处理器超过 64 个时两者可能不同（NumCPU 在旧版 Go 上只反映当前处理器组），
		// 不是错误，但要留下痕迹。
		t.Logf("提示：按处理器组统计到 %d 个线程，runtime.NumCPU() 为 %d", topo.Threads, runtime.NumCPU())
	}
	t.Logf("本机拓扑：物理核 %d，逻辑线程 %d，插槽 %d（runtime.NumCPU()=%d）",
		topo.Cores, topo.Threads, topo.Sockets, runtime.NumCPU())
}

func TestActiveProcessorCount(t *testing.T) {
	// 合成一条 RelationGroup 记录：8 字节记录头 + 24 字节 GROUP_RELATIONSHIP 头 + N 个 48 字节条目
	groupRec := func(counts ...byte) []byte {
		rec := make([]byte, 32+48*len(counts))
		binary.LittleEndian.PutUint16(rec[10:12], uint16(len(counts))) // ActiveGroupCount
		for i, c := range counts {
			rec[32+48*i+1] = c // PROCESSOR_GROUP_INFO.ActiveProcessorCount
		}
		return rec
	}

	cases := []struct {
		name string
		rec  []byte
		want int
	}{
		{"单组 16 线程", groupRec(16), 16},
		{"两组共 96 线程（>64 需要分组）", groupRec(64, 32), 96},
		{"空记录", nil, 0},
		{"长度不足头部", make([]byte, 31), 0},
		{"组数为 0", make([]byte, 80), 0},
	}
	for _, c := range cases {
		if got := activeProcessorCount(c.rec); got != c.want {
			t.Errorf("%s：得到 %d，期望 %d", c.name, got, c.want)
		}
	}
}
