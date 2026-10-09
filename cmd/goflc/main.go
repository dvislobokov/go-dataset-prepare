// Command goflc builds caret-based full-line completion samples from Go repositories.
//
//	goflc extract  --repo DIR --out DIR [--config FILE] [--repository-id ID] [--revision SHA] [--license SPDX]
//	               [--workers N] [--corpus] [--gzip] [--semantic none|best_effort|required] [--overwrite]
//	goflc validate --dataset DIR --repo DIR
//	goflc config   (prints the effective default config)
//
// Safety: input repositories are read as files only. Nothing from the input is built, executed, or downloaded:
// no go command, no go generate, no cgo, no module download (see docs/PLAN.md, "Safety tiers").
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"

	"goflc/internal/flc"
	"goflc/internal/render"
	"goflc/internal/semantic"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: goflc <command> [flags]

commands:
  extract    discovery + syntax caret extraction (+ optional semantic sidecar) into an atomic output directory
  discover   corpus pass: discovery + corpus.jsonl only (same filters), no caret extraction
  validate   exact-reconstruction / schema / leakage validation of a dataset against the repository bytes
  render     serialize samples (+ editor_snapshot facts) into flc-prompt/v2 training records
  config     print the built-in default config as JSON

run 'goflc <command> -h' for flags`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if pf := os.Getenv("GOFLC_CPUPROFILE"); pf != "" { // diagnostics only
		if f, err := os.Create(pf); err == nil {
			pprof.StartCPUProfile(f)
			defer pprof.StopCPUProfile()
		}
	}
	switch os.Args[1] {
	case "extract":
		code := cmdExtract(os.Args[2:])
		pprof.StopCPUProfile()
		os.Exit(code)
	case "validate":
		os.Exit(cmdValidate(os.Args[2:]))
	case "render":
		os.Exit(cmdRender(os.Args[2:]))
	case "discover":
		os.Exit(cmdDiscover(os.Args[2:]))
	case "config":
		b, _ := json.MarshalIndent(flc.DefaultConfig(), "", "  ")
		fmt.Println(string(b))
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func cmdExtract(args []string) int {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	repo := fs.String("repo", "", "repository checkout (read-only input)")
	out := fs.String("out", "", "output directory (written atomically)")
	cfgPath := fs.String("config", "", "JSON config (defaults: goflc config)")
	repoID := fs.String("repository-id", "", "repository id, e.g. github.com/owner/name")
	rev := fs.String("revision", "", "full commit SHA (default: read from .git/HEAD as a file)")
	lic := fs.String("license", "", "declared SPDX license id (from the selection manifest)")
	workers := fs.Int("workers", 1, "parallel extraction workers (1 = sequential baseline)")
	overwrite := fs.Bool("overwrite", false, "replace an existing output directory")
	corpus := fs.Bool("corpus", false, "also write corpus.jsonl (exact source per accepted file)")
	noCorpus := fs.Bool("no-corpus", false, "do not write corpus.jsonl (default; overrides --corpus)")
	gz := fs.Bool("gzip", false, "write *.jsonl.gz")
	sem := fs.String("semantic", "", "override semantic.mode: none|best_effort|required")
	engine := fs.String("semantic-engine", "", "override semantic.engine: auto|snapshot")
	fs.Parse(args)
	if *repo == "" || *out == "" {
		fs.Usage()
		return 2
	}
	opt := flc.RunOptions{Repo: *repo, Out: *out, ConfigPath: *cfgPath, RepositoryID: *repoID, Revision: *rev,
		License: *lic, Workers: *workers, Overwrite: *overwrite, Corpus: *corpus && !*noCorpus, Gzip: *gz,
		CommandLine: append([]string{"goflc", "extract"}, args...)}
	cfgFile := *cfgPath
	if *sem != "" || *engine != "" {
		// write an effective config with the override so the manifest records it
		cfg, _, err := flc.LoadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if *sem != "" {
			cfg.Semantic.Mode = *sem
		}
		if *engine != "" {
			cfg.Semantic.Engine = *engine
		}
		if err := cfg.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		tmp, err := os.CreateTemp("", "goflc-config-*.json")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		json.NewEncoder(tmp).Encode(cfg)
		tmp.Close()
		defer os.Remove(tmp.Name())
		cfgFile = tmp.Name()
	}
	opt.ConfigPath = cfgFile
	opt.SemanticHook = semantic.Run
	m, err := flc.Run(opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "extract:", err)
		return 1
	}
	b, _ := json.MarshalIndent(map[string]any{"out": *out, "timings": m["timings"], "throughput": m["throughput"],
		"resources": m["resources"]}, "", "  ")
	fmt.Println(string(b))
	return 0
}

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	ds := fs.String("dataset", "", "dataset directory produced by extract")
	repo := fs.String("repo", "", "repository checkout the dataset was extracted from")
	fs.Parse(args)
	if *ds == "" || *repo == "" {
		fs.Usage()
		return 2
	}
	rep, err := flc.Validate(*ds, *repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "validate:", err)
		return 1
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	_ = os.WriteFile(filepath.Join(*ds, "validation.json"), append(b, '\n'), 0o644)
	fmt.Println(string(b))
	if !rep.OK {
		return 1
	}
	return 0
}

func cmdRender(args []string) int {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	ds := fs.String("dataset", "", "dataset directory (samples.jsonl [+ semantic.jsonl])")
	out := fs.String("out", "", "output directory for prompts.<split>.jsonl")
	o := render.DefaultOptions()
	fs.IntVar(&o.MaxCodeChars, "max-code-chars", o.MaxCodeChars, "code window budget (UTF-16 units), cut from the left on a line boundary")
	fs.IntVar(&o.MaxSemanticChars, "max-semantic-chars", o.MaxSemanticChars, "semantic block budget (UTF-16 units)")
	fs.StringVar(&o.Policy, "policy", o.Policy, "semantic visibility policy to render")
	noTypes := fs.Bool("no-types", false, "omit TYPE lines (flc-prompt/v1 layout)")
	preview := fs.Int("preview", 100, "examples written to preview.md")
	fs.Parse(args)
	if *ds == "" || *out == "" {
		fs.Usage()
		return 2
	}
	if *noTypes {
		o.IncludeTypes, o.Format = false, render.FormatV1
	}
	sum, err := render.Dataset(*ds, *out, o, *preview)
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		return 1
	}
	b, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(b))
	return 0
}

func cmdDiscover(args []string) int {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	repo := fs.String("repo", "", "repository checkout (read-only input)")
	out := fs.String("out", "", "output directory (written atomically)")
	cfgPath := fs.String("config", "", "JSON config")
	repoID := fs.String("repository-id", "", "repository id")
	rev := fs.String("revision", "", "full commit SHA (default: read from .git/HEAD as a file)")
	lic := fs.String("license", "", "declared SPDX license id")
	overwrite := fs.Bool("overwrite", false, "replace an existing output directory")
	gz := fs.Bool("gzip", false, "write *.jsonl.gz")
	fs.Parse(args)
	if *repo == "" || *out == "" {
		fs.Usage()
		return 2
	}
	_, err := flc.Run(flc.RunOptions{Repo: *repo, Out: *out, ConfigPath: *cfgPath, RepositoryID: *repoID, Revision: *rev,
		License: *lic, Workers: 1, Overwrite: *overwrite, Corpus: true, Gzip: *gz, CorpusOnly: true,
		CommandLine: append([]string{"goflc", "discover"}, args...)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover:", err)
		return 1
	}
	return 0
}
