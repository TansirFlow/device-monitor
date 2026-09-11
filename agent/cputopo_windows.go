//go:build windows

package main

// Windows 侧没有 /proc、/sys，物理拓扑只能问内核。
//
// 关键区别（也是当初报错的根因）：
//
//	runtime.NumCPU() 返回的是**逻辑处理器**数——8 核 16 线程会得到 16；
//	GetLogicalProcessorInformationEx(RelationProcessorCore) 为每个**物理核心**
//	返回一条记录，同一个核心上的两个超线程共享一条。
//
// 因此物理核心数 = RelationProcessorCore 的记录条数。该调用顺带还给出了
// 插槽数（RelationProcessorPackage）与处理器组（RelationGroup）。
//
// 只调用 kernel32 的只读查询 API：不执行外部程序、不写文件、不开端口。

import (
	"encoding/binary"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	relationProcessorCore    = 0
	relationProcessorPackage = 3
	relationGroup            = 4
	relationAll              = 0xffff
	errorInsufficientBuffer  = 122
)

var procGLPIE = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLogicalProcessorInformationEx")

func cpuTopoWindows() cputopo {
	// runtime.NumCPU() 是可靠的逻辑处理器数，先垫上；后面若有更准的分组统计再覆盖。
	t := cputopo{Threads: runtime.NumCPU()}

	buf, n, ok := glpieRecords()
	if !ok {
		return t
	}

	cores, pkgs := 0, 0
	for off := 0; off+8 <= n; {
		rel := binary.LittleEndian.Uint32(buf[off:])
		size := int(binary.LittleEndian.Uint32(buf[off+4:]))
		// size 是内核给出的下一条记录的步长；<8 或越界说明缓冲区被截断，就此打住。
		if size < 8 || off+size > n {
			break
		}
		switch rel {
		case relationProcessorCore:
			cores++
		case relationProcessorPackage:
			pkgs++
		case relationGroup:
			if c := activeProcessorCount(buf[off : off+size]); c > 0 {
				t.Threads = c
			}
		}
		off += size
	}

	if cores > 0 {
		t.Cores = cores
	}
	if pkgs > 0 {
		t.Sockets = pkgs
	}
	return t
}

// glpieRecords 两段式调用：第一次只问需要多大缓冲区，第二次取数据。
// 第一次必然以 ERROR_INSUFFICIENT_BUFFER 失败，那是正常流程而非错误。
func glpieRecords() ([]byte, int, bool) {
	var need uint32
	r, _, err := procGLPIE.Call(relationAll, 0, uintptr(unsafe.Pointer(&need)))
	if r == 0 && err != syscall.Errno(errorInsufficientBuffer) {
		return nil, 0, false
	}
	if need == 0 {
		return nil, 0, false
	}
	buf := make([]byte, need)
	r, _, _ = procGLPIE.Call(relationAll,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&need)))
	if r == 0 {
		return nil, 0, false
	}
	return buf, int(need), true
}

// activeProcessorCount 解析 RelationGroup 记录里的「在线逻辑处理器总数」。
//
// 一台机器超过 64 个逻辑处理器时，Windows 会把它切成多个处理器组，
// 而 runtime.NumCPU() 在旧版 Go 上只反映当前组，会少报。这里按组累加。
//
// 记录布局（GROUP_RELATIONSHIP，紧跟在本记录 8 字节头之后）：
//
//	MaximumGroupCount(2) ActiveGroupCount(2) Reserved[20]
//	然后紧跟 ActiveGroupCount 个 PROCESSOR_GROUP_INFO，
//	每个条目的第 1 个字节是 ActiveProcessorCount。
//
// 条目长度随架构不同（32 位 40 字节 / 64 位 48 字节），因此用
// (记录长度 - 头部长度) / 组数 反算，不硬编码。
func activeProcessorCount(rec []byte) int {
	if len(rec) < 32 {
		return 0
	}
	groups := int(binary.LittleEndian.Uint16(rec[10:12]))
	if groups <= 0 {
		return 0
	}
	entry := (len(rec) - 32) / groups
	if entry < 2 {
		return 0
	}
	total := 0
	for i := 0; i < groups; i++ {
		base := 32 + i*entry
		if base+2 > len(rec) {
			break
		}
		total += int(rec[base+1])
	}
	return total
}
