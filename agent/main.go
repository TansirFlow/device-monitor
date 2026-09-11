package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// 由 -ldflags "-X main.version=..." 注入
var (
	version   = "1.0.0"
	buildTime = "unknown"
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("")

	var (
		cfgPath  = flag.String("config", "agent.json", "配置文件路径（JSON）")
		once     = flag.Bool("once", false, "采集一次并打印 JSON 后退出（不上报）")
		dryRun   = flag.Bool("dry-run", false, "采集一次并打印将要上报的内容后退出（不上报）")
		selftest = flag.Bool("selftest", false, "采集并真实上报一次，用于验证网络与鉴权")
		showVer  = flag.Bool("version", false, "打印版本信息后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("mon-agent %s (built %s)\n", version, buildTime)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatalf("配置错误: %v", err)
	}
	logf := func(format string, a ...any) {
		log.Printf("[agent "+cfg.NodeID+"] "+format, a...)
	}

	if warn := warnFilePermissions(*cfgPath); warn != "" {
		logf("警告: %s", warn)
	}
	if cfg.TLS.InsecureSkipVerify {
		logf("警告: 已开启 insecure_skip_verify，仅建议用于内网自签证书联调")
	}

	collector := newCollector(cfg)

	// 关键：先做一次「预热采集」并丢弃。
	// CPU / 网络占用率是两次采样之间的增量，没有基线时首个样本只能报 0。
	// 预热后，第一个真正上报的样本覆盖一个完整周期，数据从第一帧就是准的。
	_ = collector.Collect(context.Background())

	if *once || *dryRun {
		time.Sleep(time.Second)
		p := collector.Collect(context.Background())
		body, err := marshalPayload(p)
		if err != nil {
			fatalf("序列化失败: %v", err)
		}
		if *dryRun {
			fmt.Fprintf(os.Stderr, "# 目标: %s\n# 体积: %d 字节\n", cfg.MasterURL, len(body))
		}
		os.Stdout.Write(body)
		fmt.Println()
		return
	}

	sender, err := newSender(cfg)
	if err != nil {
		fatalf("初始化上报通道失败: %v", err)
	}

	if *selftest {
		time.Sleep(time.Second)
		p := collector.Collect(context.Background())
		body, err := marshalPayload(p)
		if err != nil {
			fatalf("序列化失败: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
		defer cancel()
		if err := sender.Send(ctx, body); err != nil {
			fatalf("自检上报失败: %v", err)
		}
		logf("自检通过：已成功上报 %d 字节到 %s（interval=%ds）", len(body), cfg.MasterURL, cfg.IntervalSec)
		return
	}

	run(cfg, collector, sender, logf)
}

// run 是主循环：定时采集 -> 入内存环形缓冲 -> 顺序发送。
// 断网时数据留在内存里待补发，不做任何落盘（避免写文件带来的攻击面）。
func run(cfg *Config, collector *Collector, sender *Sender, logf func(string, ...any)) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logf("启动完成：version=%s interval=%ds target=%s buffer=%d",
		version, cfg.IntervalSec, cfg.MasterURL, cfg.BufferMax)

	ticker := time.NewTicker(time.Duration(cfg.IntervalSec) * time.Second)
	defer ticker.Stop()

	// 有界内存缓冲：master 短暂不可用时按序补发，超出容量则丢最旧的。
	var queue []*Payload
	var dropped uint64
	var lastSize int

	var tick uint64
	for {
		select {
		case <-ctx.Done():
			logf("收到退出信号，正常结束（累计上报失败丢弃 %d 条）", dropped)
			return
		case <-ticker.C:
		}

		tick++
		p := collector.Collect(ctx)
		p.Buffered = len(queue)
		if dropped > 0 {
			p.DroppedTotal = dropped
		}
		queue = append(queue, p)
		for len(queue) > cfg.BufferMax && len(queue) > 1 {
			queue = queue[1:]
			dropped++
		}

		// 按顺序补发；一旦失败就停下，保留队列下次再试。
		for len(queue) > 0 {
			body, err := marshalPayload(queue[0])
			if err != nil {
				logf("序列化失败，丢弃该样本: %v", err)
				queue = queue[1:]
				continue
			}
			sendCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSec)*time.Second)
			err = sender.Send(sendCtx, body)
			cancel()
			if err != nil {
				if tick%20 == 1 || len(queue) == cfg.BufferMax {
					logf("上报失败（本地已缓存 %d 条，将自动重试）: %v", len(queue), err)
				}
				break
			}
			lastSize = len(body)
			queue = queue[1:]
		}

		if tick%60 == 0 {
			logf("运行中：已上报 %d 次，待补发 %d 条，最近单次 %d 字节，采集耗时 %dms",
				tick, len(queue), lastSize, p.CollectMS)
		}
		if p.Errors != nil && tick%60 == 0 {
			logf("采集降级项: %v", p.Errors)
		}
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "mon-agent: "+format+"\n", a...)
	os.Exit(1)
}
