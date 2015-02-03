package main

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	wd, err := os.Getwd()
	if len(os.Args) < 2 {
		fmt.Println("usage: unrollbench [packages]")
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
	var files []string
	for _, path := range os.Args[1:] {
		if path == "syscall" {
			// syscall is a snowflake. Leave it alone.
			continue
		}
		pkg, err := build.Import(path, wd, 0)
		if err != nil {
			fatal(err)
		}
		for _, file := range pkg.TestGoFiles {
			files = append(files, filepath.Join(pkg.Dir, file))
		}
		for _, file := range pkg.XTestGoFiles {
			files = append(files, filepath.Join(pkg.Dir, file))
		}
	}

	for _, file := range files {
		fmt.Println("Processing", file)
		fset := token.NewFileSet()
		// TODO: avoid stripping build tags
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			fatal(err)
		}
		fi, err := os.Stat(file)
		if err != nil {
			fatal(err)
		}

		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			// Find benchmark-like functions.
			// We are flexible here because we want to detect and rewrite
			// helper functions like this one from math/big:
			// 	func benchmarkBitLenN(b *testing.B, nbits uint) {
			// 		testword := Word((uint64(1) << nbits) - 1)
			// 		for i := 0; i < b.N; i++ {
			// 			bitLen(testword)
			// 		}
			// 	}
			if !ok || !isBench(fn) {
				continue
			}

			// Keep it simple: Look for top level for loops up to b.N.
			// This also makes this operation idempotent, since the
			// rewrite moves the loops inside an if/then/else statement.
			for i, s := range fn.Body.List {
				ok, id, body := isBenchForLoop(s)
				if !ok {
					continue
				}
				newfor := unrolled(s.(*ast.ForStmt), id, body)
				fn.Body.List[i] = newfor
			}
		}

		c, err := os.OpenFile(file, os.O_WRONLY|os.O_TRUNC, fi.Mode())
		if err != nil {
			fatal(err)
		}
		if err := printer.Fprint(c, fset, f); err != nil {
			fatal(err)
		}
		c.Close()
	}
}

func fatal(msg interface{}) {
	fmt.Println(msg)
	os.Exit(1)
}

// isBench reports whether n is a benchmark.
// It assumes that the testing package has been imported
// under its own name.
func isBench(n *ast.FuncDecl) bool {
	if !strings.HasPrefix(strings.ToLower(n.Name.Name), "bench") ||
		n.Type.Params == nil ||
		len(n.Type.Params.List) == 0 {
		return false
	}

	// Check that one of the params is b *testing.B.
	for _, p := range n.Type.Params.List {
		if len(p.Names) != 1 || p.Names[0].Name != "b" {
			continue
		}
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "B" {
			continue
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != "testing" {
			continue
		}
		return true
	}

	return false
}

// isBenchForLoop reports whether n a statement of the form:
//
// for i := 0; i < b.N; i++ {
//   // body
// }
//
// in which i is any ident?
// TODO: be more flexible in what we look for. (samesafeexpr)
// TODO: make sure that i is not read and b.N is not written to in the body. Or elsewhere either?
func isBenchForLoop(n ast.Stmt) (is bool, id string, body *ast.BlockStmt) {
	f, ok := n.(*ast.ForStmt)
	if !ok {
		return
	}

	if f.Init == nil || f.Cond == nil || f.Post == nil {
		return
	}

	// condition not of form a < b
	bin, ok := f.Cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.LSS {
		return
	}

	// rhs must be b.N
	sel, ok := bin.Y.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "N" {
		return
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != "b" {
		return
	}

	// i must be an ident
	i, ok := bin.X.(*ast.Ident)
	if !ok {
		return
	}

	ini, ok := f.Init.(*ast.AssignStmt)
	if !ok || len(ini.Lhs) != 1 || len(ini.Rhs) != 1 {
		return
	}

	inilhs, ok := ini.Lhs[0].(*ast.Ident)
	if !ok || inilhs.Name != i.Name {
		return
	}

	post, ok := f.Post.(*ast.IncDecStmt)
	if !ok || post.Tok != token.INC {
		return
	}
	postlhs, ok := post.X.(*ast.Ident)
	if !ok || postlhs.Name != i.Name {
		return
	}

	return true, i.Name, f.Body
}

func unrolled(f *ast.ForStmt, id string, body *ast.BlockStmt) ast.Stmt {
	// Build:
	// if b.N < 10 {
	// 	for i := 0; i < b.N; i++ {
	//		// body
	// 	}
	// } else {
	// 	for i := 0; i < b.N / 10; i++ {
	//   {
	//     // body
	//   }
	//   // repeat 9 more times
	// }

	s := &ast.IfStmt{
		Cond: &ast.BinaryExpr{
			X: ast.NewIdent("b.N"), // cheating a little
			Y: &ast.BasicLit{
				Kind:  token.INT,
				Value: "10",
			},
			Op: token.LSS,
		},
	}

	s.Body = &ast.BlockStmt{
		List: []ast.Stmt{
			&ast.ForStmt{
				Init: f.Init,
				Cond: f.Cond,
				Post: f.Post,
				Body: body,
			},
		},
	}

	var ten []ast.Stmt
	for i := 0; i < 10; i++ {
		ten = append(ten, body)
	}

	s.Else = &ast.BlockStmt{
		List: []ast.Stmt{
			&ast.ForStmt{
				Init: f.Init,
				Cond: &ast.BinaryExpr{
					X: ast.NewIdent(id),
					Y: &ast.BinaryExpr{
						Op: token.QUO,
						X:  ast.NewIdent("b.N"), // cheat
						Y: &ast.BasicLit{
							Kind:  token.INT,
							Value: "10",
						},
					},
					Op: token.LSS,
				},
				Post: f.Post,
				Body: &ast.BlockStmt{List: ten},
			},
		},
	}

	return s
}
