// Copyright 2019 The Bazel Authors. All rights reserved.
// Copyright 2021 Derivita Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bazelpackagesdriver

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/bazelbuild/bazel-watcher/bazel"
	"github.com/bazelbuild/bazel-watcher/third_party/bazel/master/src/main/protobuf/blaze_query"
	"github.com/derivita/bazelpackagesdriver/driver"
	"github.com/derivita/bazelpackagesdriver/pkgconv"
	"golang.org/x/tools/go/packages"
	"golang.org/x/xerrors"
)

const fileQueryPrefix = "file="

var gorootPattern = regexp.MustCompile(`^bazel-[^/]+/external/go_sdk\b`)

const supportedModes = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedDeps | packages.NeedTypesSizes | packages.NeedModule

type bazelDriver struct {
	cfg           driver.Request
	resp          driver.Response
	bazel         bazel.Bazel
	sdk           *gosdk
	workspaceRoot string
	stdlibImports map[string]bool
	fileQueries   map[string]bool
	importQueries map[string]bool
	wildcardQuery bool
}

// New returns a Driver implementation based on bazel query.
func New() driver.Driver {
	return func(cfg driver.Request, patterns ...string) (*driver.Response, error) {
		bzl := bazel.New()
		sdk, err := newGoSDK(bzl)
		if err != nil {
			return nil, err
		}
		driver := &bazelDriver{
			cfg:           cfg,
			bazel:         bzl,
			sdk:           sdk,
			stdlibImports: make(map[string]bool),
			fileQueries:   make(map[string]bool),
			importQueries: make(map[string]bool),
		}
		return driver.loadPackages(patterns...)
	}
}

func (d *bazelDriver) loadPackages(patterns ...string) (*driver.Response, error) {
	if info, err := d.bazel.Info(); err != nil {
		return &driver.Response{NotHandled: true}, nil
	} else {
		d.workspaceRoot = info["workspace"]
	}

	log.Printf("mode: %v", d.cfg.Mode)
	unsupportedModes := d.cfg.Mode &^ supportedModes
	if unsupportedModes != 0 {
		return nil, fmt.Errorf("%v (%b) not implemented", unsupportedModes, unsupportedModes)
	}

	if len(patterns) == 0 {
		patterns = append(patterns, ".")
	}

	var queries []string

	var ignoredPkg packages.Package

	stdlibRoot := filepath.Join(d.sdk.goroot, "src")

	for _, patt := range patterns {
		if strings.HasPrefix(patt, fileQueryPrefix) {
			fp := strings.TrimPrefix(patt, fileQueryPrefix)
			if len(fp) == 0 {
				return nil, fmt.Errorf("\"file=\" prefix given with no query after it")
			}
			if filepath.IsAbs(fp) {
				if strings.HasPrefix(fp, stdlibRoot) {
					if rel, err := filepath.Rel(stdlibRoot, fp); err == nil {
						pkg := filepath.Dir(rel)
						if d.sdk.packages[pkg] {
							d.stdlibImports[pkg] = true
						}
					}
					continue
				}
				if rel, err := filepath.Rel(d.workspaceRoot, fp); err == nil {
					fp = rel
				}
			}
			if prefix := gorootPattern.FindString(fp); prefix != "" {
				ignoredPkg.Name = prefix
				ignoredPkg.ID = prefix
				ignoredPkg.IgnoredFiles = append(ignoredPkg.IgnoredFiles, fp)
				continue
			}
			query, err := d.convertFileQuery(fp)
			if err != nil {
				return nil, err
			}
			queries = append(queries, query)
			d.fileQueries[fp] = true
		} else if patt == "." {
			queries = append(queries, goFilter(":all"))
			d.wildcardQuery = true
		} else if patt == "./..." {
			queries = append(queries, goFilter("..."))
			d.wildcardQuery = true
		} else if d.sdk.packages[patt] {
			d.stdlibImports[patt] = true
		} else {
			// TODO: do we need to check for a package ID instead of import path?
			d.importQueries[patt] = true
			queries = append(queries, fmt.Sprintf("attr(importpath, '%v', deps(%v))", patt, goFilter("//...")))
		}
	}

	var resp driver.Response

	for pkg := range d.stdlibImports {
		resp.Roots = append(resp.Roots, pkg)
	}

	if len(queries) != 0 {
		pkgs, roots, err := d.packagesFromQueries(queries)
		if err != nil {
			return nil, err
		}
		resp.Packages = append(resp.Packages, pkgs...)
		resp.Roots = append(resp.Roots, roots...)
	}

	if len(ignoredPkg.IgnoredFiles) > 0 {
		log.Printf("Ignored %v files", len(ignoredPkg.IgnoredFiles))
		ignoredPkg.GoFiles = ignoredPkg.IgnoredFiles[:1]
		ignoredPkg.CompiledGoFiles = ignoredPkg.GoFiles
		ignoredPkg.IgnoredFiles = ignoredPkg.IgnoredFiles[1:]
		resp.Packages = append(resp.Packages, &ignoredPkg)
		resp.Roots = append(resp.Roots, ignoredPkg.ID)
	}

	stdlib, err := d.sdk.loadPackages(&d.cfg, d.stdlibImports)
	if err != nil {
		return nil, err
	}
	resp.Packages = append(resp.Packages, stdlib...)

	if d.cfg.Mode&packages.NeedTypesSizes != 0 {
		goarch := driver.GetEnv(&d.cfg, "GOARCH", runtime.GOARCH)
		// SizesFor always return StdSizesm,
		resp.Sizes = types.SizesFor("gc", goarch).(*types.StdSizes)
	}

	return &resp, nil
}

func (d *bazelDriver) convertFileQuery(path string) (string, error) {
	log.Printf("bazel query %#v", path)
	result, err := d.bazel.Query(path)
	if err != nil {
		return "", err
	}
	if len(result.Target) == 0 {
		return "", fmt.Errorf("no source file %#v", path)
	} else if len(result.Target) > 1 {
		return "", fmt.Errorf("multiple results for %#v", path)
	}

	t := result.Target[0]
	if t.GetType() != blaze_query.Target_SOURCE_FILE {
		return "", fmt.Errorf("wanted SOURCE_FILE, but %v is %v", path, t.GetType())
	}
	name := t.GetSourceFile().GetName()
	pkg := strings.Split(name, ":")[0]
	return fmt.Sprintf("attr(srcs, '%v', '%v:*')", name, pkg), nil
}

func (d *bazelDriver) packagesFromQueries(queries []string) (pkgs []*packages.Package, roots []string, err error) {
	query := strings.Join(queries, "+")
	if d.cfg.Mode&packages.NeedDeps != 0 {
		query = goFilter(fmt.Sprintf("deps(%v)", query))
	}

	log.Printf("bazel query %#v", query)
	results, err := d.bazel.Query(query)
	if err != nil {
		return
	}

	pkgs = pkgconv.Load(results.GetTarget())

	log.Printf("file queries: %v", d.fileQueries)
	for _, p := range pkgs {
		if d.includeInRoots(p) {
			roots = append(roots, p.ID)
		}
	}

	if d.cfg.Mode&packages.NeedCompiledGoFiles != 0 {
		for _, pkg := range pkgs {
			pkg.CompiledGoFiles = pkg.GoFiles
		}
	}
	if d.cfg.Mode&(packages.NeedImports|packages.NeedName) != 0 {
		for _, pkg := range pkgs {
			name, imports, err := d.parsePackage(pkg)
			if err != nil {
				return nil, nil, xerrors.Errorf("parsePackage: %w", err)
			}
			pkg.Name = name
			for _, i := range imports {
				if i, err = strconv.Unquote(i); err == nil {
					if d.sdk.packages[i] {
						if pkg.Imports == nil {
							pkg.Imports = make(map[string]*packages.Package)
						}
						pkg.Imports[i] = &packages.Package{ID: i}
						d.stdlibImports[i] = true
					}
				}
			}
		}
	}
	return
}

func (d *bazelDriver) includeInRoots(pkg *packages.Package) bool {
	if d.wildcardQuery {
		return strings.HasPrefix(pkg.ID, "//")
	}
	if pkg.PkgPath != "" && d.importQueries[pkg.PkgPath] {
		return true
	} else if len(d.fileQueries) > 0 && !strings.HasPrefix(pkg.ID, "@") {
		for _, f := range pkg.GoFiles {
			if d.fileQueries[f] {
				return true
			}
			if rel, err := filepath.Rel(d.workspaceRoot, f); err == nil && d.fileQueries[rel] {
				return true
			}
		}
	}
	return false
}

func (d *bazelDriver) parsePackage(pkg *packages.Package) (packageName string, imports []string, err error) {
	triedBuild := false
	dedupe := make(map[string]bool)
	fset := token.NewFileSet()

	for _, filename := range pkg.GoFiles {
		var f *os.File
		f, err = os.Open(filename)
		if os.IsNotExist(err) && !triedBuild {
			triedBuild = true
			log.Printf("bazel build %v\n", pkg.ID)
			d.bazel.Build(pkg.ID)
			f, err = os.Open(filename)
		}
		if err != nil {
			return
		}
		defer f.Close()
		var syntax *ast.File
		if syntax, err = parser.ParseFile(fset, filename, f, parser.ImportsOnly); err == nil {
			packageName = syntax.Name.Name
			for _, i := range syntax.Imports {
				pkg := i.Path.Value
				if !dedupe[pkg] {
					dedupe[pkg] = true
					imports = append(imports, pkg)
				}
			}
			if d.cfg.Mode&packages.NeedImports == 0 {
				// We don't need to parse all the files just to get the package name.
				return
			}
		}
	}
	if packageName == "" {
		packageName = filepath.Base(pkg.PkgPath)
	}
	return
}

func goFilter(expr string) string {
	return fmt.Sprintf("kind(\"alias|proto_library|go_\", %s)", expr)
}
