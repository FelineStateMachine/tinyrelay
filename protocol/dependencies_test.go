package protocol_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestPublicProtocolDependencyGraph(t *testing.T) {
	for _, name := range []string{"nostr", "auth"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "list", "-deps", "-f", "{{.ImportPath}}", "./"+name)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("dependency graph: %v\n%s", err, out)
			}
			found := false
			for _, dependency := range strings.Fields(string(out)) {
				if dependency == modulePath+"/protocol/"+name {
					found = true
				}
				if strings.HasPrefix(dependency, modulePath+"/") && dependency != modulePath+"/protocol/nostr" && dependency != modulePath+"/protocol/auth" {
					t.Errorf("protocol/%s depends on host package %s", name, dependency)
				}
				for _, forbidden := range []string{"modernc.org/sqlite", "github.com/prometheus/", "go.opentelemetry.io/"} {
					if strings.HasPrefix(dependency, forbidden) {
						t.Errorf("protocol/%s depends on host infrastructure %s", name, dependency)
					}
				}
			}
			if !found {
				t.Fatal("dependency graph omitted target package")
			}
		})
	}
}

// Keep this generic package's public surface deliberate: feature-specific
// kind constants, private-kind classification and room/job semantics belong
// to their owning packages, not the Nostr wire model.
func TestNostrPublicSurface(t *testing.T) {
	want := strings.Fields("Event Filter Validate Parse Canonical Tag TagValues Expiration Difficulty CommittedDifficulty Sign GenerateKey PublicKey IsEphemeral IsReplaceable IsAddressable ParseFilter Matches SearchTerms Filter.MarshalJSON")
	var got []string
	files, err := filepath.Glob("nostr/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if !decl.Name.IsExported() {
					continue
				}
				name := decl.Name.Name
				if decl.Recv != nil {
					receiver := decl.Recv.List[0].Type
					if pointer, ok := receiver.(*ast.StarExpr); ok {
						receiver = pointer.X
					}
					name = receiver.(*ast.Ident).Name + "." + name
				}
				got = append(got, name)
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if spec.Name.IsExported() {
							got = append(got, spec.Name.Name)
						}
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							if name.IsExported() {
								got = append(got, name.Name)
							}
						}
					}
				}
			}
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Nostr public API changed; review protocol scope\ngot: %v\nwant: %v", got, want)
	}
}
