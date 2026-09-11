package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Sender 是 agent 唯一的对外通道：一次 HTTP POST，然后读走并丢弃响应体。
//
// 它**没有任何**监听端口，也不存在任何解析 master 响应内容的逻辑——
// 响应只被用来判断 2xx/非 2xx。即使 master 完全被攻陷，
// 它能对节点产生的影响上限就是「返回一个状态码」。
type Sender struct {
	cfg    *Config
	client *http.Client
	target string
}

func newSender(cfg *Config) (*Sender, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.TLS.InsecureSkipVerify, // 仅供自签证书的内网联调
		ServerName:         cfg.TLS.ServerName,
	}

	if cfg.TLS.CAFile != "" {
		pem, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("读取 CA 文件失败: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA 文件 %s 中没有可用的 PEM 证书", cfg.TLS.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.TLS.CertFile != "" || cfg.TLS.KeyFile != "" {
		if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
			return nil, fmt.Errorf("启用 mTLS 需要同时配置 cert_file 与 key_file")
		}
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("加载客户端证书失败: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	var proxy func(*http.Request) (*url.URL, error)
	if cfg.UseEnvProxy {
		proxy = http.ProxyFromEnvironment
	}

	tr := &http.Transport{
		TLSClientConfig:       tlsCfg,
		Proxy:                 proxy,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   time.Duration(cfg.TimeoutSec) * time.Second,
		// 安全：不跟随重定向。否则一个被攻陷/被劫持的 master 可以把上报
		// 重定向到任意第三方地址，造成指标与令牌外泄。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &Sender{cfg: cfg, client: client, target: cfg.MasterURL}, nil
}

// Send 上报一条快照。body 由调用方预先序列化，便于记录体积。
func (s *Sender) Send(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("User-Agent", "mon-agent/"+version)
	req.Header.Set("X-Node-Id", s.cfg.NodeID)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 响应内容一律丢弃，绝不解析成指令。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("master 返回 %s", resp.Status)
	}
	return nil
}

func marshalPayload(p *Payload) ([]byte, error) {
	return json.Marshal(p)
}
