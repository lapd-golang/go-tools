package unused // import "honnef.co/go/unused"

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"os"
	"strings"

	"golang.org/x/tools/go/loader"
)

type CheckMode int

const (
	CheckConstants CheckMode = 1 << iota
	CheckFields
	CheckFunctions
	CheckTypes
	CheckVariables
)

type Checker struct {
	Mode    CheckMode
	Fset    *token.FileSet
	Verbose bool
}

func (c *Checker) checkConstants() bool { return (c.Mode & CheckConstants) > 0 }
func (c *Checker) checkFields() bool    { return (c.Mode & CheckFields) > 0 }
func (c *Checker) checkFunctions() bool { return (c.Mode & CheckFunctions) > 0 }
func (c *Checker) checkTypes() bool     { return (c.Mode & CheckTypes) > 0 }
func (c *Checker) checkVariables() bool { return (c.Mode & CheckVariables) > 0 }

func (c *Checker) Check(paths []string) ([]types.Object, error) {
	// We resolve paths manually instead of relying on go/loader so
	// that our TypeCheckFuncBodies implementation continues to work.
	err := resolveRelative(paths)
	if err != nil {
		return nil, err
	}
	defs := map[types.Object]bool{}
	var interfaces []*types.Interface
	var unused []types.Object

	conf := loader.Config{}
	if !c.Verbose {
		conf.TypeChecker.Error = func(error) {}
	}
	pkgs := map[string]bool{}
	for _, path := range paths {
		pkgs[path] = true
		pkgs[path+"_test"] = true
	}
	conf.TypeCheckFuncBodies = func(s string) bool {
		return pkgs[s]
	}
	for _, path := range paths {
		conf.ImportWithTests(path)
	}
	lprog, err := conf.Load()
	if err != nil {
		return nil, err
	}

	for _, pkg := range lprog.InitialPackages() {
		for _, obj := range pkg.Defs {
			if obj == nil {
				continue
			}
			if obj, ok := obj.(*types.Var); ok {
				if typ, ok := obj.Type().(*types.Interface); ok {
					interfaces = append(interfaces, typ)
				}
			}
			if obj, ok := obj.(*types.TypeName); ok {
				if typ, ok := obj.Type().Underlying().(*types.Interface); ok {
					interfaces = append(interfaces, typ)
				}
			}
			if isVariable(obj) && !isPkgScope(obj) && !isField(obj) {
				// Skip variables that aren't package variables or struct fields
				continue
			}
			if _, ok := obj.(*types.PkgName); ok {
				continue
			}
			defs[obj] = false
		}
		for _, obj := range pkg.Uses {
			defs[obj] = true
		}
		for _, file := range pkg.Files {
			var v visitor
			v = func(node ast.Node) ast.Visitor {
				if node, ok := node.(*ast.CompositeLit); ok {
					var obj types.Type
					if _, ok := node.Type.(*ast.StructType); ok {
						obj = pkg.TypeOf(node)
					} else {
						ident, ok := node.Type.(*ast.Ident)
						if !ok {
							return v
						}
						obj, ok = pkg.ObjectOf(ident).Type().(*types.Named)
						if !ok {
							return v
						}
					}
					typ, ok := obj.Underlying().(*types.Struct)
					if !ok {
						return v
					}
					basic := false
					for _, elt := range node.Elts {
						if _, ok := elt.(*ast.KeyValueExpr); !ok {
							basic = true
							break
						}
					}
					if basic {
						n := typ.NumFields()
						for i := 0; i < n; i++ {
							field := typ.Field(i)
							defs[field] = true
						}
					}
				}
				return v
			}
			ast.Walk(v, file)
		}
	}
	for obj, used := range defs {
		if obj.Pkg() == nil {
			continue
		}
		// TODO methods + reflection
		if !c.checkFlags(obj) {
			continue
		}
		if used {
			continue
		}
		if obj.Name() == "_" {
			continue
		}
		if obj.Exported() && (isPkgScope(obj) || isMethod(obj) || isField(obj)) {
			f := lprog.Fset.Position(obj.Pos()).Filename
			if !strings.HasSuffix(f, "_test.go") ||
				strings.HasPrefix(obj.Name(), "Test") ||
				strings.HasPrefix(obj.Name(), "Benchmark") ||
				strings.HasPrefix(obj.Name(), "Example") {
				continue
			}
		}
		if isMain(obj) {
			continue
		}
		if isFunction(obj) && !isMethod(obj) && obj.Name() == "init" {
			continue
		}
		if isMethod(obj) && implements(obj, interfaces) {
			continue
		}
		unused = append(unused, obj)
	}
	c.Fset = lprog.Fset
	return unused, nil
}

func resolveRelative(importPaths []string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	for i, path := range importPaths {
		bpkg, err := build.Import(path, wd, build.FindOnly)
		if err != nil {
			return fmt.Errorf("can't load package %q: %v", path, err)
		}
		importPaths[i] = bpkg.ImportPath
	}
	return nil
}

func Check(paths []string, flags CheckMode) ([]types.Object, error) {
	checker := Checker{Mode: flags}
	return checker.Check(paths)
}

func implements(obj types.Object, ifaces []*types.Interface) bool {
	recvType := obj.(*types.Func).Type().(*types.Signature).Recv().Type()
	for _, iface := range ifaces {
		if !types.Implements(recvType, iface) {
			continue
		}
		n := iface.NumMethods()
		for i := 0; i < n; i++ {
			if iface.Method(i).Name() == obj.Name() {
				return true
			}
		}
	}
	return false
}

func isPkgScope(obj types.Object) bool {
	return obj.Parent() == obj.Pkg().Scope()
}

func isMain(obj types.Object) bool {
	if obj.Pkg().Name() != "main" {
		return false
	}
	if obj.Name() != "main" {
		return false
	}
	if !isPkgScope(obj) {
		return false
	}
	if !isFunction(obj) {
		return false
	}
	if isMethod(obj) {
		return false
	}
	return true
}

func isFunction(obj types.Object) bool {
	_, ok := obj.(*types.Func)
	return ok
}

func isMethod(obj types.Object) bool {
	if !isFunction(obj) {
		return false
	}
	return obj.(*types.Func).Type().(*types.Signature).Recv() != nil
}

func isVariable(obj types.Object) bool {
	_, ok := obj.(*types.Var)
	return ok
}

func isConstant(obj types.Object) bool {
	_, ok := obj.(*types.Const)
	return ok
}

func isType(obj types.Object) bool {
	_, ok := obj.(*types.TypeName)
	return ok
}

func isField(obj types.Object) bool {
	if obj, ok := obj.(*types.Var); ok && obj.IsField() {
		return true
	}
	return false
}

func (c *Checker) checkFlags(obj types.Object) bool {
	if isFunction(obj) && !c.checkFunctions() {
		return false
	}
	if isVariable(obj) && !c.checkVariables() {
		return false
	}
	if isConstant(obj) && !c.checkConstants() {
		return false
	}
	if isType(obj) && !c.checkTypes() {
		return false
	}
	if isField(obj) && !c.checkFields() {
		return false
	}
	return true
}

type visitor func(node ast.Node) ast.Visitor

func (v visitor) Visit(node ast.Node) ast.Visitor {
	return v(node)
}
