// Package semantic computes caret facts with go/types on target-free snapshots.
//
// Import resolution is OFFLINE and never runs the go command or any input code:
//   - standard library: parsed from $GOROOT/src (the builder's own toolchain), build context fixed, cgo disabled;
//   - packages of the analyzed module: resolved through the root go.mod module path to repository directories;
//   - ./vendor/<path> when present (read-only parse);
//   - everything else (external modules) is UNRESOLVED: go/types sees an import error, uses of that package have
//     invalid types, and records are reason-coded partially_resolved (external_imports_unresolved).
//
// go/build is used only for file selection (build constraints) through ImportDir with custom ReadDir/OpenFile
// hooks; with those hooks set go/build never invokes the go command.
package semantic

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

type loadedPkg struct {
	pkg   *types.Package
	err   error
	nerrs int
}

// Loader type-checks imported packages once per process (per repository run) and caches them.
type Loader struct {
	mu         sync.Mutex // guards cache/inProgress/Unresolved/Stats; held for the whole load of an import graph
	Fset       *token.FileSet
	ctx        build.Context
	goroot     string
	repoRoot   string
	modulePath string
	useVendor  bool
	cache      map[string]*loadedPkg // import path -> package
	inProgress map[string]bool
	Unresolved map[string]int // import path -> count of failed imports
	Stats      map[string]int
}

func NewLoader(repoRoot string, modulePath *string, goos, goarch string, tags []string, useVendor bool) *Loader {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH = goos, goarch
	ctx.CgoEnabled = false
	ctx.BuildTags = append([]string{}, tags...)
	ctx.GOPATH = ""
	// Custom file hooks: (1) go/build will not shell out to `go list` when any hook is set; (2) reads stay plain.
	ctx.ReadDir = func(dir string) ([]fs.FileInfo, error) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		out := make([]fs.FileInfo, 0, len(ents))
		for _, e := range ents {
			if e.Type()&fs.ModeSymlink != 0 {
				continue // never follow symlinks out of the checkout
			}
			if fi, err := e.Info(); err == nil {
				out = append(out, fi)
			}
		}
		return out, nil
	}
	ctx.OpenFile = func(p string) (io.ReadCloser, error) { return os.Open(p) }
	goroot := ctx.GOROOT
	if goroot == "" {
		goroot = runtime.GOROOT()
	}
	l := &Loader{Fset: token.NewFileSet(), ctx: ctx, goroot: goroot, repoRoot: repoRoot, useVendor: useVendor,
		cache: map[string]*loadedPkg{}, inProgress: map[string]bool{}, Unresolved: map[string]int{}, Stats: map[string]int{}}
	if modulePath != nil {
		l.modulePath = *modulePath
	}
	return l
}

func (l *Loader) isStd(path string) bool {
	first := strings.SplitN(path, "/", 2)[0]
	if strings.Contains(first, ".") {
		return false
	}
	st, err := os.Stat(filepath.Join(l.goroot, "src", filepath.FromSlash(path)))
	return err == nil && st.IsDir()
}

// dirFor resolves an import path to a directory, or "" when it cannot be resolved offline.
func (l *Loader) dirFor(path, fromDir string) (dir string, origin string) {
	if l.isStd(path) {
		return filepath.Join(l.goroot, "src", filepath.FromSlash(path)), "std"
	}
	if strings.HasPrefix(fromDir, filepath.Join(l.goroot, "src")) {
		d := filepath.Join(l.goroot, "src", "vendor", filepath.FromSlash(path))
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d, "std_vendor"
		}
	}
	if l.modulePath != "" && (path == l.modulePath || strings.HasPrefix(path, l.modulePath+"/")) {
		rel := strings.TrimPrefix(strings.TrimPrefix(path, l.modulePath), "/")
		d := filepath.Join(l.repoRoot, filepath.FromSlash(rel))
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d, "module"
		}
	}
	if l.useVendor {
		d := filepath.Join(l.repoRoot, "vendor", filepath.FromSlash(path))
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d, "vendor"
		}
	}
	return "", ""
}

var errUnresolved = errors.New("unresolved import (offline: external module not available)")

// ImportFrom implements types.ImporterFrom. Safe for concurrent use by many snapshot checks: the cache is locked,
// and a missing package (with its whole import graph) is loaded while the lock is held.
func (l *Loader) Import(path string) (*types.Package, error) { return l.ImportFrom(path, "", 0) }

func (l *Loader) ImportFrom(path, fromDir string, _ types.ImportMode) (*types.Package, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.importLocked(path, fromDir)
}

// lockedImporter is used for checks that run while l.mu is already held (nested imports during a load).
type lockedImporter struct{ l *Loader }

func (li lockedImporter) Import(path string) (*types.Package, error) {
	return li.l.importLocked(path, "")
}
func (li lockedImporter) ImportFrom(path, fromDir string, _ types.ImportMode) (*types.Package, error) {
	return li.l.importLocked(path, fromDir)
}

// Stat increments a loader statistic (thread-safe).
func (l *Loader) Stat(k string, n int) {
	l.mu.Lock()
	l.Stats[k] += n
	l.mu.Unlock()
}

func (l *Loader) importLocked(path, fromDir string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if path == "C" {
		l.Unresolved["C"]++
		return nil, errUnresolved
	}
	dir, origin := l.dirFor(path, fromDir)
	if dir == "" {
		l.Unresolved[path]++
		return nil, errUnresolved
	}
	key := dir
	if lp, ok := l.cache[key]; ok {
		l.Stats["import_cache_hits"]++
		return lp.pkg, lp.err
	}
	if l.inProgress[key] {
		return nil, fmt.Errorf("import cycle through %s", path)
	}
	l.inProgress[key] = true
	defer delete(l.inProgress, key)
	l.Stats["packages_loaded_"+origin]++
	// Imported packages are only needed for their declarations: function bodies are dropped before checking
	// (what export data would contain). This is the main load-time and memory saving.
	files, _, err := l.parseDir(dir, false, true)
	if err != nil && len(files) == 0 {
		lp := &loadedPkg{err: err}
		l.cache[key] = lp
		return nil, err
	}
	nerr := 0
	conf := types.Config{Importer: lockedImporter{l}, Error: func(error) { nerr++ }, FakeImportC: true}
	pkg, _ := conf.Check(path, l.Fset, files, nil)
	if pkg != nil {
		pkg.MarkComplete()
	}
	lp := &loadedPkg{pkg: pkg, nerrs: nerr}
	if nerr > 0 {
		l.Stats["imported_packages_with_type_errors"]++
	}
	l.cache[key] = lp
	return pkg, nil
}

// PkgFiles lists the build-matching files of a directory under the fixed context.
type PkgFiles struct {
	Name        string
	GoFiles     []string // non-test files
	TestGoFiles []string // _test.go files of the same package
	XTestFiles  []string // package x_test files
}

func (l *Loader) ListDir(dir string) (*PkgFiles, error) {
	bp, err := l.ctx.ImportDir(dir, build.ImportComment)
	if bp == nil {
		return nil, err
	}
	var noGo *build.NoGoError
	if err != nil && !errors.As(err, &noGo) {
		var multi *build.MultiplePackageError
		if !errors.As(err, &multi) {
			// partial info is still usable
		}
	}
	pf := &PkgFiles{Name: bp.Name}
	for _, f := range bp.GoFiles {
		pf.GoFiles = append(pf.GoFiles, filepath.Join(dir, f))
	}
	for _, f := range bp.TestGoFiles {
		pf.TestGoFiles = append(pf.TestGoFiles, filepath.Join(dir, f))
	}
	for _, f := range bp.XTestGoFiles {
		pf.XTestFiles = append(pf.XTestFiles, filepath.Join(dir, f))
	}
	sort.Strings(pf.GoFiles)
	sort.Strings(pf.TestGoFiles)
	sort.Strings(pf.XTestFiles)
	return pf, nil
}

func (l *Loader) parseDir(dir string, withTests, stripBodies bool) ([]*ast.File, *PkgFiles, error) {
	pf, err := l.ListDir(dir)
	if pf == nil {
		return nil, nil, err
	}
	names := pf.GoFiles
	if withTests {
		names = append(append([]string{}, names...), pf.TestGoFiles...)
	}
	var files []*ast.File
	for _, n := range names {
		f, perr := parser.ParseFile(l.Fset, n, nil, parser.SkipObjectResolution)
		if f != nil {
			if stripBodies {
				StripBodies(f, token.NoPos)
			}
			files = append(files, f)
		}
		_ = perr
	}
	return files, pf, nil
}

// ParseFiles parses the given absolute paths once (shared FileSet) for reuse across many snapshot checks. The other
// files of the analyzed package are only needed for package-level declarations, so their function bodies are dropped
// (the edited function is the only body re-bound per caret).
func (l *Loader) ParseFiles(paths []string) map[string]*ast.File {
	out := map[string]*ast.File{}
	for _, p := range paths {
		if f, _ := parser.ParseFile(l.Fset, p, nil, parser.SkipObjectResolution); f != nil {
			StripBodies(f, token.NoPos)
			out[p] = f
		}
	}
	return out
}

// StripBodies drops the bodies of all function declarations except the one whose range contains keep. Package-level
// facts (scopes, types, method sets) do not depend on function bodies, so this is fact-preserving for everything
// outside the edited function. Package-level var initializers (incl. func literals) are kept.
func StripBodies(f *ast.File, keep token.Pos) {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			if keep.IsValid() && fd.Pos() <= keep && keep <= fd.End() {
				continue
			}
			fd.Body = nil
		}
	}
}

// skeletonDecl returns d, or for a function declaration a shallow copy without its body (cached ASTs stay intact).
func skeletonDecl(d ast.Decl) ast.Decl {
	if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
		c := *fd
		c.Body = nil
		return &c
	}
	return d
}

// ImportPathOf returns the module import path for a repository-relative package directory ("" if no module).
func (l *Loader) ImportPathOf(relDir string) string {
	if l.modulePath == "" {
		return "main/" + relDir
	}
	if relDir == "." || relDir == "" {
		return l.modulePath
	}
	return l.modulePath + "/" + relDir
}
