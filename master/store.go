package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nodeIDRe 是与 agent 完全一致的白名单。
// 这是防止路径穿越的**第二道**防线（第一道在 agent 配置校验）。
var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const dayLayout = "2006-01-02"

// Store 是内存索引 + 按天追加的 JSONL 落盘。
//
// 为什么不用 SQLite：单机监控的写入模式是「纯追加 + 按时间范围读」，
// JSONL 天然匹配，而且零外部依赖、二进制更小、故障时可以直接 cat 排查。
// 历史日期会在维护任务中自动压缩为 .jsonl.gz（体积约 1/10）。
type Store struct {
	dir          string
	cachePerNode int

	mu     sync.RWMutex
	latest map[string]*Sample
	recent map[string][]*Sample // 每节点最近 N 条，供看板秒开
	order  []string
}

func NewStore(dir string, cachePerNode int) *Store {
	if cachePerNode < 60 {
		cachePerNode = 60
	}
	return &Store{
		dir:          dir,
		cachePerNode: cachePerNode,
		latest:       map[string]*Sample{},
		recent:       map[string][]*Sample{},
	}
}

func (s *Store) Dir() string { return s.dir }

// within 兜底校验目标路径确实位于 root 之下。
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func dayPath(dir, node, day string, gz bool) string {
	name := day + ".jsonl"
	if gz {
		name += ".gz"
	}
	return filepath.Join(dir, node, name)
}

// Append 落盘一条样本并刷新内存缓存。
func (s *Store) Append(sm *Sample) error {
	if sm == nil {
		return fmt.Errorf("空样本")
	}
	if !nodeIDRe.MatchString(sm.NodeID) {
		return fmt.Errorf("node_id 不合法")
	}
	day := time.Unix(sm.TS, 0).Format(dayLayout)
	nodeDir := filepath.Join(s.dir, sm.NodeID)
	p := dayPath(s.dir, sm.NodeID, day, false)
	if !within(s.dir, p) {
		return fmt.Errorf("路径越界")
	}
	if err := os.MkdirAll(nodeDir, 0o750); err != nil {
		return err
	}

	line, err := json.Marshal(sm)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	s.mu.Lock()
	if _, ok := s.latest[sm.NodeID]; !ok {
		s.order = append(s.order, sm.NodeID)
	}
	s.latest[sm.NodeID] = sm
	r := append(s.recent[sm.NodeID], sm)
	if len(r) > s.cachePerNode {
		r = r[len(r)-s.cachePerNode:]
	}
	s.recent[sm.NodeID] = r
	s.mu.Unlock()
	return nil
}

// Nodes 返回全部节点最新状态，按 node_id 排序，附带在线判定。
func (s *Store) Nodes(now int64) []*NodeView {
	s.mu.RLock()
	out := make([]*NodeView, 0, len(s.latest))
	for id, sm := range s.latest {
		out = append(out, s.viewOf(id, sm, now))
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func (s *Store) viewOf(id string, sm *Sample, now int64) *NodeView {
	stale := int64(30)
	if sm.IntervalSec > 0 {
		stale = int64(sm.IntervalSec) * 3
	}
	age := now - sm.TS
	if age < 0 {
		age = 0
	}
	return &NodeView{
		Sample:        *sm,
		AgeSec:        age,
		StaleAfterSec: stale,
		Online:        age <= stale,
		Temp:          SummarizeTemps(sm),
	}
}

// Node 返回单个节点最新状态。
func (s *Store) Node(id string, now int64) (*NodeView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sm, ok := s.latest[id]
	if !ok {
		return nil, false
	}
	return s.viewOf(id, sm, now), true
}

// KnownNodes 返回所有已知节点 id。
func (s *Store) KnownNodes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.latest))
	for id := range s.latest {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Range 返回 [from, to] 区间内某节点的原始样本（升序）。
// 优先命中内存缓存，避免看板刷新时反复读盘。
func (s *Store) Range(node string, from, to int64) ([]*Sample, error) {
	if !nodeIDRe.MatchString(node) {
		return nil, fmt.Errorf("node_id 不合法")
	}

	s.mu.RLock()
	cached := s.recent[node]
	s.mu.RUnlock()

	if len(cached) > 0 && cached[0].TS <= from {
		var out []*Sample
		for _, sm := range cached {
			if sm.TS >= from && sm.TS <= to {
				out = append(out, sm)
			}
		}
		return out, nil
	}

	var out []*Sample
	cur := time.Unix(from, 0).Truncate(24 * time.Hour)
	end := time.Unix(to, 0)
	for !cur.After(end) {
		day := cur.Format(dayLayout)
		p := dayPath(s.dir, node, day, false)
		if !fileExists(p) {
			p = dayPath(s.dir, node, day, true)
		}
		if fileExists(p) {
			err := readSamples(p, func(sm *Sample) bool {
				if sm.TS > to {
					return false
				}
				if sm.TS >= from {
					out = append(out, sm)
				}
				return true
			})
			if err != nil {
				return nil, err
			}
		}
		cur = cur.Add(24 * time.Hour)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// readSamples 逐行读取（可自动处理 .gz），fn 返回 false 时提前结束。
func readSamples(path string, fn func(*Sample) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}

	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = trimSpaceBytes(line)
			if len(line) > 0 {
				var sm Sample
				if json.Unmarshal(line, &sm) == nil {
					if !fn(&sm) {
						return nil
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func trimSpaceBytes(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// LoadLatest 在启动时从磁盘恢复「每节点最新一条」，让重启后看板不空白。
func (s *Store) LoadLatest() (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, os.MkdirAll(s.dir, 0o750)
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !nodeIDRe.MatchString(e.Name()) {
			continue
		}
		nodeDir := filepath.Join(s.dir, e.Name())
		files, err := os.ReadDir(nodeDir)
		if err != nil {
			continue
		}
		var names []string
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
				names = append(names, f.Name())
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		last := filepath.Join(nodeDir, names[len(names)-1])

		var lastSample *Sample
		if err := readSamples(last, func(sm *Sample) bool {
			cp := *sm
			lastSample = &cp
			return true
		}); err != nil || lastSample == nil {
			continue
		}
		s.mu.Lock()
		s.latest[lastSample.NodeID] = lastSample
		s.order = append(s.order, lastSample.NodeID)
		s.mu.Unlock()
		n++
	}
	return n, nil
}

// Maintain 做两件事：把历史日期压缩成 .gz、清理超过保留期的数据。
// 只在自己的数据目录内操作，且严格按文件名日期判断。
func (s *Store) Maintain(retentionDays int, now time.Time) (compressed, deleted int, err error) {
	if retentionDays < 1 {
		retentionDays = 30
	}
	today := now.Format(dayLayout)
	cutoff := now.AddDate(0, 0, -retentionDays).Format(dayLayout)

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, e := range entries {
		if !e.IsDir() || !nodeIDRe.MatchString(e.Name()) {
			continue
		}
		nodeDir := filepath.Join(s.dir, e.Name())
		files, err := os.ReadDir(nodeDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			var day string
			isGz := false
			switch {
			case strings.HasSuffix(name, ".jsonl.gz"):
				day = strings.TrimSuffix(name, ".jsonl.gz")
				isGz = true
			case strings.HasSuffix(name, ".jsonl"):
				day = strings.TrimSuffix(name, ".jsonl")
			default:
				continue
			}
			if _, perr := time.Parse(dayLayout, day); perr != nil {
				continue
			}
			full := filepath.Join(nodeDir, name)
			if !within(s.dir, full) {
				continue
			}

			if day < cutoff {
				if err := os.Remove(full); err == nil {
					deleted++
				}
				continue
			}
			// 非今天的未压缩文件 → 压缩归档
			if !isGz && day < today {
				if err := gzipFile(full, full+".gz"); err == nil {
					_ = os.Remove(full)
					compressed++
				}
			}
		}
	}
	return compressed, deleted, nil
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer out.Close()

	zw, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return err
	}
	zw.Name = filepath.Base(src)
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// parseInt64 小工具，供 API 层解析查询参数。
func parseInt64(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}
