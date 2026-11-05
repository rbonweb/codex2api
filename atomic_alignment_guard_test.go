package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// On 32-bit platforms (arm/386/mips) only the first 8 bytes of an allocated object are guaranteed aligned, so a bare int64/uint64
// field in the middle of a struct passed to atomic.AddInt64 and similar functions panics at run time with "unaligned 64-bit atomic operation".
// CI only runs 64-bit, so this class of bug is never visible; this test therefore bans this set of functions on struct fields at the source level,
// and everything should use atomic.Int64/atomic.Uint64 instead (the compiler guarantees their alignment).
var legacy64BitAtomicFuncs = map[string]bool{
	"AddInt64": true, "AddUint64": true,
	"LoadInt64": true, "LoadUint64": true,
	"StoreInt64": true, "StoreUint64": true,
	"SwapInt64": true, "SwapUint64": true,
	"CompareAndSwapInt64": true, "CompareAndSwapUint64": true,
}

func TestNoLegacy64BitAtomicsOnStructFields(t *testing.T) {
	var violations []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "frontend" {
				return filepath.SkipDir
			}
			// Nested independent modules (such as reference implementation copies) do not belong to this binary.
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		atomicName := ""
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != "sync/atomic" {
				continue
			}
			atomicName = "atomic"
			if imp.Name != nil {
				atomicName = imp.Name.Name
			}
		}
		if atomicName == "" || atomicName == "_" {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !legacy64BitAtomicFuncs[fn.Sel.Name] {
				return true
			}
			if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != atomicName {
				return true
			}
			addr, ok := call.Args[0].(*ast.UnaryExpr)
			if !ok || addr.Op != token.AND {
				return true
			}
			if _, isField := ast.Unparen(addr.X).(*ast.SelectorExpr); isField {
				violations = append(violations, fset.Position(call.Pos()).String()+": atomic."+fn.Sel.Name+" on a struct field; use atomic.Int64/atomic.Uint64")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("legacy 64-bit atomics on struct fields panic on 32-bit platforms:\n%s", strings.Join(violations, "\n"))
	}
}
