package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	version   = "1.0.0"
	buildTime = "unknown"
)

func main() {
	log.SetFlags(log.LstdFlags)

	var (
		cfgPath = flag.String("config", "master.json", "配置文件路径（JSON）")
		listen  = flag.String("listen", "", "覆盖监听地址，例如 0.0.0.0:8080")
		dataDir = flag.String("data-dir", "", "覆盖数据目录")
		showVer = flag.Bool("version", false, "打印版本信息后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("mon-master %s (built %s)\n", version, buildTime)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatalf("配置错误: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	// 命令行覆盖后必须重新校验，避免绕过「非回环监听必须带 admin_token」的检查。
	if err := cfg.validate(); err != nil {
		fatalf("配置错误: %v", err)
	}

	store := NewStore(cfg.DataDir, cfg.CachePerNode)
	if n, err := store.LoadLatest(); err != nil {
		log.Printf("恢复历史最新状态失败（不影响启动）: %v", err)
	} else if n > 0 {
		log.Printf("已从磁盘恢复 %d 个节点的最新状态", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 管理后台的两个存储：账号（admin.json）与节点令牌/安装码（nodes.json）。
	// 都落在 data_dir 下 —— 那是主控唯一的可写目录，主控绝不改写自己的配置文件。
	admin, err := ensureAdmin(cfg.DataDir)
	if err != nil {
		fatalf("初始化管理员账号失败: %v", err)
	}
	nodes := newNodeStore(cfg.DataDir, cfg)
	if err := nodes.load(); err != nil {
		fatalf("加载节点令牌失败: %v", err)
	}

	go maintenanceLoop(ctx, store, cfg)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           NewServer(cfg, store, webHandler(), admin, nodes).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(os.Stderr, "http: ", log.LstdFlags),
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("mon-master %s 已启动，监听 %s，数据目录 %s，保留 %d 天",
			version, cfg.Listen, cfg.DataDir, cfg.RetentionDays)
		if cfg.AdminToken == "" {
			log.Printf("提示：未设置 admin_token，看板仅对本机开放（配置校验已强制回环监听）")
		}
		log.Printf("管理后台：http://%s/admin.html  （管理员账号 %q）", cfg.Listen, admin.username())
		if cfg.AgentDir == "" {
			log.Printf("提示：未配置 agent_dir，一键安装只会生成脚本、不提供客户端二进制下载")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		fatalf("服务异常退出: %v", err)
	case <-ctx.Done():
		log.Printf("收到退出信号，正在优雅关闭…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("关闭超时: %v", err)
	}
	log.Printf("已退出")
}

// maintenanceLoop 周期性地压缩历史文件并清理过期数据。
func maintenanceLoop(ctx context.Context, store *Store, cfg *Config) {
	run := func() {
		compressed, deleted, err := store.Maintain(cfg.RetentionDays, time.Now())
		if err != nil {
			log.Printf("维护任务失败: %v", err)
			return
		}
		if compressed > 0 || deleted > 0 {
			log.Printf("维护完成：压缩 %d 个历史文件，清理 %d 个过期文件", compressed, deleted)
		}
	}
	run() // 启动时先跑一次，把上一次运行残留的未压缩文件处理掉

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "mon-master: "+format+"\n", a...)
	os.Exit(1)
}
