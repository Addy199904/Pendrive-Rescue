package disk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceNeverAsksForWriteAccess reads every source file of this package
// (for every operating system) and fails if any of them asks for write
// access: a write flag, a write call, or a raw Windows handle opened with
// anything but GENERIC_READ. The rest of the program only reaches a drive
// through this package's Source, which has no way to write.
func TestSourceNeverAsksForWriteAccess(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	bannedNames := map[string]bool{
		"O_RDWR": true, "O_WRONLY": true, "O_CREATE": true, "O_APPEND": true, "O_TRUNC": true,
		"GENERIC_WRITE": true, "GENERIC_ALL": true, "FILE_WRITE_DATA": true, "FILE_APPEND_DATA": true,
	}
	bannedCalls := map[string]bool{
		"os.Create": true, "os.WriteFile": true, "os.Truncate": true, "os.Remove": true, "os.RemoveAll": true,
		"syscall.WriteFile": true, "syscall.Write": true, "syscall.Pwrite": true, "syscall.Ftruncate": true,
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if bannedNames[x.Name] {
					t.Errorf("%s: write access requested (%s)", fset.Position(x.Pos()), x.Name)
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && bannedCalls[pkg.Name+"."+x.Sel.Name] {
					t.Errorf("%s: write call %s.%s", fset.Position(x.Pos()), pkg.Name, x.Sel.Name)
				}
				if x.Sel.Name == "WriteAt" || x.Sel.Name == "Truncate" {
					t.Errorf("%s: write call .%s", fset.Position(x.Pos()), x.Sel.Name)
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "openRaw" && len(x.Args) == 2 {
					// allowed: GENERIC_READ, or 0 (query only, no data access)
					sel, isSel := x.Args[1].(*ast.SelectorExpr)
					lit, isLit := x.Args[1].(*ast.BasicLit)
					if !(isSel && sel.Sel.Name == "GENERIC_READ") && !(isLit && lit.Value == "0") {
						t.Errorf("%s: openRaw must be called with syscall.GENERIC_READ or 0 only", fset.Position(x.Pos()))
					}
				}
			}
			return true
		})
	}
	if checked < 4 {
		t.Fatalf("only %d source files checked - run the test from the package folder", checked)
	}
}

// TestImageIsOpenedReadOnly checks at run time that an image file opened as
// a source cannot be written through the handle the program holds.
func TestImageIsOpenedReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stick.img")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0644); err != nil {
		t.Fatal(err)
	}
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	img, ok := src.(*imageSource)
	if !ok {
		t.Fatalf("Open returned %T for an image file", src)
	}
	if _, err := img.f.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("the image handle accepted a write")
	}
}
