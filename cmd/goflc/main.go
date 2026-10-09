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

	"goflc/internal/flc"
	"goflc/internal/semantic"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: goflc <command> [flags]

commands:
  extract    discovery + syntax caret extraction (+ optional semantic sidecar) into an atomic output directory
  validate   exact-reconstruction / schema / leakage validation of a dataset against the repository bytes
  config     print the built-in default config as JSON

run 'goflc <command> -h' for flags`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "extract":
		os.Exit(cmdExtract(os.Args[2:]))
	case "validate":
		os.Exit(cmdValidate(os.Args[2:]))
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
	gz := fs.Bool("gzip", false, "write *.jsonl.gz")
	sem := fs.String("semantic", "", "override semantic.mode: none|best_effort|required")
	fs.Parse(args)
	if *repo == "" || *out == "" {
		fs.Usage()
		return 2
	}
	opt := flc.RunOptions{Repo: *repo, Out: *out, ConfigPath: *cfgPath, RepositoryID: *repoID, Revision: *rev,
		License: *lic, Workers: *workers, Overwrite: *overwrite, Corpus: *corpus, Gzip: *gz,
		CommandLine: append([]string{"goflc", "extract"}, args...)}
	cfgFile := *cfgPath
	if *sem != "" {
		// write an effective config with the override so the manifest records it
		cfg, _, err := flc.LoadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cfg.Semantic.Mode = *sem
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
