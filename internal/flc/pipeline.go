package flc

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunOptions configures one repository extraction run.
type RunOptions struct {
	Repo, Out    string
	ConfigPath   string
	RepositoryID string
	Revision     string // optional; resolved from .git/HEAD (read as a file) when empty
	License      string // declared SPDX id (optional)
	Workers      int
	Overwrite    bool
	Corpus       bool
	Gzip         bool
	SemanticHook func(dir string, cfg *Config, repo *RepoInfo) (map[string]int, error) // optional semantic pass
	CommandLine  []string
}

// JSONLWriter writes one JSON value per line (HTML escaping off), optionally gzip-compressed.
type JSONLWriter struct {
	f   *os.File
	gz  *gzip.Writer
	w   *bufio.Writer
	enc *json.Encoder
	N   int
}

func NewJSONLWriter(path string, gz bool) (*JSONLWriter, error) {
	if gz {
		path += ".gz"
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	j := &JSONLWriter{f: f}
	var w io.Writer = f
	if gz {
		j.gz = gzip.NewWriter(f)
		j.gz.ModTime = time.Time{} // deterministic bytes
		w = j.gz
	}
	j.w = bufio.NewWriterSize(w, 1<<20)
	j.enc = json.NewEncoder(j.w)
	j.enc.SetEscapeHTML(false)
	return j, nil
}

func (j *JSONLWriter) Write(v any) error { j.N++; return j.enc.Encode(v) }

func (j *JSONLWriter) Close() error {
	if err := j.w.Flush(); err != nil {
		return err
	}
	if j.gz != nil {
		if err := j.gz.Close(); err != nil {
			return err
		}
	}
	return j.f.Close()
}

// OpenJSONL opens path or path.gz for reading.
func OpenJSONL(path string) (io.ReadCloser, error) {
	if f, err := os.Open(path); err == nil {
		return f, nil
	}
	f, err := os.Open(path + ".gz")
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{gz, f}, nil
}

// ResolveRevision reads HEAD from the .git directory as plain files (git is not executed on the input checkout).
func ResolveRevision(repo string) *string {
	gitDir := filepath.Join(repo, ".git")
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return nil
	}
	h := strings.TrimSpace(string(head))
	if !strings.HasPrefix(h, "ref: ") {
		if isSha(h) {
			return &h
		}
		return nil
	}
	ref := strings.TrimPrefix(h, "ref: ")
	if b, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		s := strings.TrimSpace(string(b))
		if isSha(s) {
			return &s
		}
	}
	if b, err := os.ReadFile(filepath.Join(gitDir, "packed-refs")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[1] == ref && isSha(f[0]) {
				return &f[0]
			}
		}
	}
	return nil
}

func isSha(s string) bool {
	if len(s) != 40 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

type stageTimer struct {
	mu sync.Mutex
	ns map[string]int64
}

func (t *stageTimer) add(stage string, d time.Duration) {
	t.mu.Lock()
	t.ns[stage] += int64(d)
	t.mu.Unlock()
}

// Run executes discovery -> syntax extraction (-> optional semantic hook) and writes an atomic output directory.
func Run(opt RunOptions) (map[string]any, error) {
	t0 := time.Now()
	cfg, _, err := LoadConfig(opt.ConfigPath)
	if err != nil {
		return nil, err
	}
	if opt.RepositoryID != "" {
		cfg.RepositoryID = opt.RepositoryID
	}
	if opt.Workers <= 0 {
		opt.Workers = 1
	}
	if _, err := os.Stat(opt.Out); err == nil && !opt.Overwrite {
		return nil, fmt.Errorf("output %s exists (use --overwrite)", opt.Out)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", strings.TrimRight(opt.Out, "/"), os.Getpid())
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	repo := &RepoInfo{Root: opt.Repo, RepositoryID: cfg.RepositoryID}
	if opt.Revision != "" {
		repo.Revision = &opt.Revision
	} else {
		repo.Revision = ResolveRevision(opt.Repo)
	}
	var declared *string
	if opt.License != "" {
		declared = &opt.License
	}
	repo.License, repo.LicenseReason, repo.LicenseAllowed = ResolveLicense(&cfg, opt.Repo, declared)
	repo.ModulePath, repo.GoVersion = ParseGoMod(opt.Repo)

	gzipOut := opt.Gzip
	disc, err := NewJSONLWriter(filepath.Join(tmp, "discovery.jsonl"), gzipOut)
	if err != nil {
		return nil, err
	}
	samples, err := NewJSONLWriter(filepath.Join(tmp, "samples.jsonl"), gzipOut)
	if err != nil {
		return nil, err
	}
	excl, err := NewJSONLWriter(filepath.Join(tmp, "exclusions.jsonl"), gzipOut)
	if err != nil {
		return nil, err
	}
	var corpus *JSONLWriter
	if opt.Corpus {
		if corpus, err = NewJSONLWriter(filepath.Join(tmp, "corpus.jsonl"), gzipOut); err != nil {
			return nil, err
		}
	}

	timer := &stageTimer{ns: map[string]int64{}}
	counters := map[string]int{}
	discCounters := map[string]int{} // owned by the discovery goroutine until results are drained
	var inputBytes int64
	type job struct {
		seq int
		af  *AcceptedFile
	}
	type result struct {
		seq int
		r   *FileResult
		dur time.Duration
	}
	jobs := make(chan job, opt.Workers*2)
	results := make(chan result, opt.Workers*2)
	inflight := make(chan struct{}, opt.Workers*4) // bounds the reorder buffer
	ext := NewExtractor(&cfg, repo, opt.Corpus)
	disco := NewDiscoverer(&cfg)

	var discErr error
	go func() {
		seq := 0
		tDisc := time.Now()
		var inDecide time.Duration
		discErr = disco.Discover(opt.Repo, repo, func(af *AcceptedFile) {
			inDecide += time.Since(tDisc)
			inflight <- struct{}{}
			jobs <- job{seq, af}
			seq++
			tDisc = time.Now()
		}, func(rec *DiscoveryRecord) {
			_ = disc.Write(rec)
			discCounters["files.discovered"]++
			if rec.Accepted {
				discCounters["files.accepted"]++
				discCounters["bytes.accepted"] += int(rec.Bytes)
				if rec.IsTest {
					discCounters["files.accepted_test"]++
				}
				if rec.BuildMatch != nil && !*rec.BuildMatch {
					discCounters["files.accepted_build_excluded"]++
				}
				if rec.Cgo {
					discCounters["files.accepted_cgo"]++
				}
			} else {
				discCounters["files.skipped."+*rec.SkipReason]++
			}
			inputBytes += rec.Bytes
		})
		inDecide += time.Since(tDisc)
		timer.add("discovery", inDecide)
		close(jobs)
	}()
	var wg sync.WaitGroup
	for w := 0; w < opt.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				st := time.Now()
				r := ext.Extract(j.af)
				results <- result{j.seq, r, time.Since(st)}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	pending := map[int]result{}
	next := 0
	seenDedup := map[string]bool{}
	var latencies []float64
	writeStart := time.Duration(0)
	for r := range results {
		pending[r.seq] = r
		for {
			x, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			<-inflight
			ws := time.Now()
			latencies = append(latencies, float64(x.dur.Microseconds())/1000.0)
			timer.add("syntax", x.dur)
			for k, v := range x.r.Counters {
				counters[k] += v
			}
			for i, s := range x.r.Samples {
				if cfg.Sampling.DropDuplicateLineTargets {
					if seenDedup[x.r.DedupKeys[i]] {
						counters["samples.dropped_duplicate"]++
						continue
					}
					seenDedup[x.r.DedupKeys[i]] = true
				}
				if err := samples.Write(s); err != nil {
					return nil, err
				}
				counters["samples.total"]++
				counters["samples.kind."+s.CaretKind]++
				if s.IsTest {
					counters["samples.test_code"]++
				}
				for _, f := range s.QualityFlags {
					counters["samples.flag."+f]++
				}
			}
			for _, ex := range x.r.Exclusions {
				_ = excl.Write(ex)
			}
			if corpus != nil && x.r.Corpus != nil {
				_ = corpus.Write(x.r.Corpus)
			}
			writeStart += time.Since(ws)
		}
	}
	timer.add("write", writeStart)
	for k, v := range discCounters {
		counters[k] += v
	}
	if discErr != nil {
		return nil, discErr
	}
	for _, w := range []*JSONLWriter{disc, samples, excl, corpus} {
		if w != nil {
			if err := w.Close(); err != nil {
				return nil, err
			}
		}
	}
	semCounters := map[string]int{}
	if opt.SemanticHook != nil && cfg.Semantic.Mode != "none" {
		st := time.Now()
		sc, err := opt.SemanticHook(tmp, &cfg, repo)
		timer.add("semantic", time.Since(st))
		if err != nil {
			return nil, fmt.Errorf("semantic: %w", err)
		}
		semCounters = sc
	}

	summary := map[string]any{
		"schema_version": "run-summary/v1",
		"repository_id":  repo.RepositoryID,
		"revision":       repo.Revision,
		"license":        repo.License,
		"license_reason": repo.LicenseReason,
		"module_path":    repo.ModulePath,
		"config_sha256":  cfg.Sha256(),
		"counters":       counters,
		"semantic":       semCounters,
	}
	if err := writeJSON(filepath.Join(tmp, "summary.json"), summary); err != nil {
		return nil, err
	}
	sort.Float64s(latencies)
	wall := time.Since(t0)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	stages := map[string]float64{}
	for k, v := range timer.ns {
		stages[k+"_cpu_ms"] = float64(v) / 1e6
	}
	manifest := map[string]any{
		"schema_version": "run-manifest/v1",
		"generator":      Generator,
		"command_line":   opt.CommandLine,
		"repository_id":  repo.RepositoryID,
		"revision":       repo.Revision,
		"repo_root":      opt.Repo,
		"license":        repo.License,
		"license_reason": repo.LicenseReason,
		"module_path":    repo.ModulePath,
		"go_directive":   repo.GoVersion,
		"seed":           cfg.Seed,
		"semantic_mode":  cfg.Semantic.Mode,
		"config":         cfg,
		"config_sha256":  cfg.Sha256(),
		"workers":        opt.Workers,
		"started_at":     t0.UTC().Format(time.RFC3339),
		"environment":    environment(),
		"timings": map[string]any{
			"wall_ms": float64(wall.Microseconds()) / 1000.0,
			"stages":  stages,
			"file_latency_ms": map[string]float64{"p50": pct(latencies, 0.5), "p95": pct(latencies, 0.95),
				"p99": pct(latencies, 0.99), "max": pct(latencies, 1.0)},
		},
		"resources": map[string]any{
			"peak_rss_bytes":         peakRSS(),
			"go_heap_sys_bytes":      ms.HeapSys,
			"go_total_alloc_bytes":   ms.TotalAlloc,
			"input_bytes_discovered": inputBytes,
		},
		"throughput": map[string]float64{
			"samples_per_sec":    float64(counters["samples.total"]) / wall.Seconds(),
			"source_mib_per_sec": float64(counters["bytes.accepted"]) / (1 << 20) / wall.Seconds(),
			"files_per_sec":      float64(counters["files.accepted"]) / wall.Seconds(),
		},
	}
	checksums := map[string]string{}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(tmp, e.Name()))
		if err == nil {
			checksums[e.Name()] = Sha256Hex(b)
		}
	}
	manifest["outputs"] = checksums
	if err := writeJSON(filepath.Join(tmp, "run-manifest.json"), manifest); err != nil {
		return nil, err
	}
	// atomic publish
	if _, err := os.Stat(opt.Out); err == nil {
		old := fmt.Sprintf("%s.old-%d", strings.TrimRight(opt.Out, "/"), os.Getpid())
		if err := os.Rename(opt.Out, old); err != nil {
			return nil, err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(tmp, opt.Out); err != nil {
		return nil, err
	}
	ok = true
	return manifest, nil
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted)-1) + 0.5)
	return sorted[i]
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func peakRSS() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			var kb int64
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:")), "%d", &kb)
			return kb * 1024
		}
	}
	return -1
}

func environment() map[string]any {
	env := map[string]any{"go_runtime": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"num_cpu": runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				env["cpu_model"] = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				break
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				env["mem_total"] = strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:"))
			}
		}
	}
	if out, err := exec.Command("uname", "-sr").Output(); err == nil {
		env["kernel"] = strings.TrimSpace(string(out))
	}
	return env
}
