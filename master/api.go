package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg   *Config
	store *Store
	limit *rateLimiter
	start time.Time
	web   http.Handler
}

func NewServer(cfg *Config, store *Store, web http.Handler) *Server {
	return &Server{
		cfg:   cfg,
		store: store,
		limit: newRateLimiter(cfg.RateLimitPerSec),
		start: time.Now(),
		web:   web,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/report", s.handleReport)
	mux.HandleFunc("/api/v1/nodes", s.handleNodes)
	mux.HandleFunc("/api/v1/history", s.handleHistory)
	mux.HandleFunc("/api/v1/meta", s.handleMeta)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/", s.web)
	return s.withCommonHeaders(mux)
}

func (s *Server) withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			// 看板是纯静态 + 自绘图表，不需要任何外部资源。
			h.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- 上报入口（唯一写入通道） ----------

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 POST"})
		return
	}
	if !s.limit.allow("ip:" + s.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "请求过于频繁"})
		return
	}

	maxBytes := int64(s.cfg.MaxBodyKB) * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败"})
		return
	}
	if int64(len(body)) > maxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "请求体超过限制"})
		return
	}

	var sm Sample
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&sm); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 解析失败"})
		return
	}
	if !nodeIDRe.MatchString(sm.NodeID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "node_id 不合法"})
		return
	}

	// 令牌与节点绑定：node_tokens 模式下，A 节点的令牌无法伪造 B 节点的数据。
	expected, known := s.cfg.tokenOf(sm.NodeID)
	if !known {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该节点未授权"})
		return
	}
	if !constTimeEqual(bearerToken(r), expected) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "上报令牌无效"})
		return
	}
	if !s.limit.allow("node:" + sm.NodeID) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "上报频率超限"})
		return
	}

	now := time.Now().Unix()
	// 时间戳异常一律归一到服务端时间：避免被用来污染历史区间或绕过保留期。
	if sm.TS <= 0 || sm.TS > now+300 || sm.TS < now-86400 {
		sm.TS = now
	}
	if sm.IntervalSec <= 0 || sm.IntervalSec > 3600 {
		sm.IntervalSec = 0
	}
	if len(sm.Errors) > 32 {
		sm.Errors = sm.Errors[:32]
	}

	if err := s.store.Append(&sm); err != nil {
		log.Printf("写入失败 node=%s: %v", sm.NodeID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "服务端写入失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": now})
}

// ---------- 只读查询接口 ----------

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireRead(w, r) {
		return
	}
	now := time.Now().Unix()
	writeJSON(w, http.StatusOK, map[string]any{
		"title":       s.cfg.Title,
		"server_time": now,
		"nodes":       s.store.Nodes(now),
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireRead(w, r) {
		return
	}
	q := r.URL.Query()
	node := q.Get("node")
	if !nodeIDRe.MatchString(node) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "node 参数不合法"})
		return
	}
	metric := q.Get("metric")
	if metric == "" {
		metric = "cpu"
	}
	if len(metric) > 32 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "metric 参数不合法"})
		return
	}
	mount := q.Get("mount")

	minutes := int64(60)
	if v, ok := parseInt64(q.Get("minutes")); ok && v > 0 {
		minutes = v
	}
	if minutes > 60*24*14 {
		minutes = 60 * 24 * 14 // 最多 14 天
	}

	now := time.Now().Unix()
	to := now
	if v, ok := parseInt64(q.Get("to")); ok && v > 0 && v <= now+60 {
		to = v
	}
	from := to - minutes*60

	// 自动步长：让返回点数落在 ~300 个左右，既够画图又不爆带宽。
	step := int64(0)
	if v, ok := parseInt64(q.Get("step")); ok && v >= 1 && v <= 86400 {
		step = v
	}
	if step <= 0 {
		step = (to - from) / 300
		if step < 1 {
			step = 1
		}
	}

	samples, err := s.store.Range(node, from, to)
	if err != nil {
		log.Printf("读取历史失败 node=%s: %v", node, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读取历史数据失败"})
		return
	}

	resp := HistoryResp{
		Node:   node,
		Metric: metric,
		Unit:   metricUnit(metric),
		Step:   step,
		From:   from,
		To:     to,
		Points: bucketize(samples, from, to, step, func(sm *Sample) (float64, bool) {
			return resolveMetric(sm, metric, mount)
		}),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":           "mon-master",
		"version":        version,
		"title":          s.cfg.Title,
		"server_time":    time.Now().Unix(),
		"uptime_sec":     int64(time.Since(s.start).Seconds()),
		"retention_days": s.cfg.RetentionDays,
		"read_auth":      s.cfg.AdminToken != "",
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// ---------- 指标解析与降采样 ----------

func resolveMetric(sm *Sample, metric, mount string) (float64, bool) {
	// 温度指标与 gpu 的索引语法不共用冒号，先单独解析。
	//   temp        全局最高温
	//   temp:cpu    仅 CPU 类传感器里的最高温
	if kind, ok := tempMetricKind(metric); ok {
		return tempValue(sm, kind)
	}

	if base, idx, ok := splitIndexSuffix(metric); ok {
		switch base {
		case "gpu":
			return gpuValue(sm, idx, func(g GPUStat) float64 { return g.UtilPct })
		case "gpu_mem":
			return gpuValue(sm, idx, func(g GPUStat) float64 { return g.MemUsedPct })
		case "gpu_temp":
			return gpuValue(sm, idx, func(g GPUStat) float64 { return g.TempC })
		case "gpu_power":
			return gpuValue(sm, idx, func(g GPUStat) float64 { return g.PowerW })
		}
		return 0, false
	}

	switch metric {
	case "cpu":
		if sm.CPU == nil {
			return 0, false
		}
		return sm.CPU.UsagePct, true
	case "load1":
		if sm.CPU == nil {
			return 0, false
		}
		return sm.CPU.Load1, true
	case "cpu_temp":
		if sm.CPU == nil {
			return 0, false
		}
		return sm.CPU.TempC, true
	case "mem":
		if sm.Mem == nil {
			return 0, false
		}
		return sm.Mem.UsedPct, true
	case "mem_used":
		if sm.Mem == nil {
			return 0, false
		}
		return round1(float64(sm.Mem.UsedBytes) / (1 << 30)), true
	case "disk":
		if sm.Disk == nil {
			return 0, false
		}
		if mount != "" {
			for _, m := range sm.Disk.Mounts {
				if m.Mount == mount {
					return m.UsedPct, true
				}
			}
			return 0, false
		}
		// 默认取「最紧张的那个挂载点」，比容量加权平均更有告警价值
		return sm.Disk.MaxUsedPct, true
	case "disk_avail":
		if sm.Disk == nil || mount == "" {
			return 0, false
		}
		for _, m := range sm.Disk.Mounts {
			if m.Mount == mount {
				return round1(float64(m.AvailBytes) / (1 << 30)), true
			}
		}
		return 0, false
	case "net_rx":
		if sm.Net == nil {
			return 0, false
		}
		return round1(sm.Net.RxBytesSec / 1024), true
	case "net_tx":
		if sm.Net == nil {
			return 0, false
		}
		return round1(sm.Net.TxBytesSec / 1024), true
	}
	return 0, false
}

// ---------- 温度 ----------

// 温度类别的规范取值。与 agent 的 TempKind* 常量是同一套词汇表，
// 两个模块各自独立发布，所以只能靠约定保持一致（master 不 import agent）。
// 这个列表用于文档与默认展示顺序，**不用于校验**——原因见 tempMetricKind。
const (
	TempKindCPU   = "cpu"
	TempKindGPU   = "gpu"
	TempKindDisk  = "disk"
	TempKindBoard = "board"
	TempKindNIC   = "nic"
	TempKindOther = "other"
)

// tempMetricKind 解析温度指标名。
// 返回 ("", true) 表示"全部类别的最高温"，返回 ("cpu", true) 表示只统计 CPU 类。
//
// 这里刻意**不做类别白名单**：agent 与 master 是两个独立发布的二进制，
// 若 master 只认写死的几个 kind，agent 一旦新增类别（比如以后的 "vrm"），
// 查询会静默返回一张空图而不是报错，排查成本极高。
// 因此只要求形如 temp:<小写短标识符>；真正没见过的类别在看板上会落到"其他"。
// 指标名本身已有长度上限（32），且只参与字符串比较、不参与任何路径拼接，
// 所以放宽校验不会带来新的攻击面。
func tempMetricKind(metric string) (string, bool) {
	if metric == "temp" {
		return "", true
	}
	if !strings.HasPrefix(metric, "temp:") {
		return "", false
	}
	k := metric[len("temp:"):]
	if k == "" || len(k) > 24 {
		return "", false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return "", false
		}
	}
	return k, true
}

// tempValue 取指定类别的最高温。温度是"越高越坏"的指标，
// 所以这里一律用最大值而不是平均值——平均值会把某一块过热的盘/卡抹平。
//
// 除了 temps 列表，还会回看 CPU.TempC / GPU[].TempC：
// 老版本 agent（v1.0.x）只上报这两个字段，这样升级期看板不会直接空白。
func tempValue(sm *Sample, kind string) (float64, bool) {
	best := 0.0
	found := false
	consider := func(v float64) {
		if v <= 0 || v > 200 { // 明显异常的读数直接丢弃，避免污染图表
			return
		}
		if !found || v > best {
			best, found = v, true
		}
	}

	for _, t := range sm.Temps {
		if kind != "" && t.Kind != kind {
			continue
		}
		consider(t.TempC)
	}
	if kind == "" || kind == "gpu" {
		for _, g := range sm.GPU {
			consider(g.TempC)
		}
	}
	if kind == "" || kind == "cpu" {
		if sm.CPU != nil {
			consider(sm.CPU.TempC)
		}
	}
	if kind == "" && sm.MaxTempC > 0 {
		consider(sm.MaxTempC)
	}
	if !found {
		return 0, false
	}
	return round1(best), true
}

// TempSummary 是 /api/v1/nodes 里附带的一行温度概览，方便看板直接渲染热力图条。
type TempSummary struct {
	MaxC      float64            `json:"max_c"`
	MaxName   string             `json:"max_name,omitempty"`
	MaxKind   string             `json:"max_kind,omitempty"`
	ByKind    map[string]float64 `json:"by_kind,omitempty"`
	SensorNum int                `json:"sensor_num"`
}

// SummarizeTemps 把一次采样的温度列表压成一行概览。没有任何温度传感器时返回 nil。
func SummarizeTemps(sm *Sample) *TempSummary {
	if len(sm.Temps) == 0 && sm.MaxTempC <= 0 {
		return nil
	}
	out := &TempSummary{SensorNum: len(sm.Temps), ByKind: map[string]float64{}}
	for _, t := range sm.Temps {
		if t.TempC <= 0 || t.TempC > 200 {
			continue
		}
		if v, ok := out.ByKind[t.Kind]; !ok || t.TempC > v {
			out.ByKind[t.Kind] = t.TempC
		}
		if t.TempC > out.MaxC {
			out.MaxC = t.TempC
			out.MaxName = t.Name
			out.MaxKind = t.Kind
		}
	}
	if out.MaxC <= 0 && sm.MaxTempC > 0 {
		out.MaxC = sm.MaxTempC
	}
	if len(out.ByKind) == 0 {
		out.ByKind = nil
	}
	return out
}

// splitIndexSuffix 解析 "gpu:1" 这类带索引的指标名。
func splitIndexSuffix(metric string) (string, int, bool) {
	i := strings.IndexByte(metric, ':')
	if i < 0 {
		switch metric {
		case "gpu", "gpu_mem", "gpu_temp", "gpu_power":
			return metric, -1, true
		}
		return "", 0, false
	}
	base := metric[:i]
	n, err := strconv.Atoi(metric[i+1:])
	if err != nil {
		return "", 0, false
	}
	switch base {
	case "gpu", "gpu_mem", "gpu_temp", "gpu_power":
		return base, n, true
	}
	return "", 0, false
}

func gpuValue(sm *Sample, idx int, pick func(GPUStat) float64) (float64, bool) {
	if len(sm.GPU) == 0 {
		return 0, false
	}
	if idx >= 0 {
		for _, g := range sm.GPU {
			if g.Index == idx {
				return pick(g), true
			}
		}
		return 0, false
	}
	// 未指定索引时取所有 GPU 的最大值，能反映"卡住了"的那一块
	best := pick(sm.GPU[0])
	for _, g := range sm.GPU[1:] {
		if v := pick(g); v > best {
			best = v
		}
	}
	return best, true
}

func metricUnit(metric string) string {
	if _, ok := tempMetricKind(metric); ok {
		return "°C"
	}
	base := metric
	if i := strings.IndexByte(metric, ':'); i >= 0 {
		base = metric[:i]
	}
	switch base {
	case "cpu", "mem", "disk", "gpu", "gpu_mem":
		return "%"
	case "cpu_temp", "gpu_temp":
		return "°C"
	case "gpu_power":
		return "W"
	case "net_rx", "net_tx":
		return "KiB/s"
	case "mem_used", "disk_avail":
		return "GiB"
	case "load1":
		return ""
	}
	return ""
}

// bucketize 按 step 秒分桶取平均，把原始样本压缩成图形点数。
func bucketize(samples []*Sample, from, to, step int64, val func(*Sample) (float64, bool)) []Point {
	if step < 1 {
		step = 1
	}
	type acc struct {
		sum float64
		n   int
	}
	buckets := map[int64]*acc{}
	for _, sm := range samples {
		if sm.TS < from || sm.TS > to {
			continue
		}
		v, ok := val(sm)
		if !ok {
			continue
		}
		b := (sm.TS - from) / step
		a := buckets[b]
		if a == nil {
			a = &acc{}
			buckets[b] = a
		}
		a.sum += v
		a.n++
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		a := buckets[k]
		out = append(out, Point{TS: from + k*step + step/2, V: round2(a.sum / float64(a.n))})
	}
	return out
}

// ---------- 工具 ----------

func (s *Server) requireRead(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return true // 已由 config.validate() 保证此时只监听回环地址
	}
	tk := bearerToken(r)
	if tk == "" {
		tk = r.URL.Query().Get("token")
	}
	if !constTimeEqual(tk, s.cfg.AdminToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "需要访问令牌"})
		return false
	}
	return true
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func constTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}
