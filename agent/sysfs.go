package main

// sysfs / procfs 读取的公共设施，被温度解析（tempsysfs.go）
// 与 CPU 拓扑解析（cputopo.go）共用。
//
// 本文件**故意不加 build tag**：这里全是「按约定文件名读一个文本文件」的纯逻辑，
// 与操作系统无关，只有挂载前缀不同。加了 linux tag 就等于放弃在 Windows/macOS
// 上测试它们——而 CI 和大多数开发机恰恰不是 Linux。
// 在非 Linux 平台这些函数无人引用，会被链接器整个丢掉，不进最终二进制。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sysRoot 是 sysfs 的挂载前缀。生产环境就是 /sys。
//
// 写成变量而不是常量，是为了让单元测试把它指向临时目录里搭出的假 sysfs 树，
// 从而在任意平台上真实地跑通解析规则（见 tempsysfs_test.go / cputopo_test.go）。
var sysRoot = "/sys"

// sysfsPath 拼接 sysfs 下的路径。写成函数而不是常量拼接，
// 是为了让 sysRoot 可以在测试里被替换。
func sysfsPath(elem ...string) string {
	return filepath.Join(append([]string{sysRoot}, elem...)...)
}

// readTrimmed 读取文本文件并去掉首尾空白。读不到返回空串。
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readInt 读取一个十进制整数。文件不存在、内容为空或不是数字都算失败，
// 由调用方决定如何降级——不要在这里悄悄返回 0 冒充一个真实读数。
func readInt(path string) (int, error) {
	s := readTrimmed(path)
	if s == "" {
		return 0, fmt.Errorf("读不到或内容为空: %s", path)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s 不是整数（%q）: %w", path, s, err)
	}
	return n, nil
}
