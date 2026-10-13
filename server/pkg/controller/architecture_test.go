package controller

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLayerDependencies(t *testing.T) {
	t.Parallel()
	const module = "github.com/41tair/slopless/pkg/"
	layers := []struct {
		name      string
		forbidden []string
	}{
		{name: "controller", forbidden: []string{module + "data", "entgo.io/ent", module + "tasks"}},
		{name: "service", forbidden: []string{module + "controller", module + "data/ent", "entgo.io/ent", "net/http"}},
		{name: "data", forbidden: []string{module + "controller", module + "service", "net/http"}},
		{name: "model", forbidden: []string{module + "controller", module + "service", module + "data", "net/http"}},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			dir := filepath.Join("..", layer.name)
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
					continue
				}
				path := filepath.Join(dir, file.Name())
				source, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
				if err != nil {
					t.Fatal(err)
				}
				for _, imported := range source.Imports {
					dependency, err := strconv.Unquote(imported.Path.Value)
					if err != nil {
						t.Fatal(err)
					}
					for _, forbidden := range layer.forbidden {
						if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
							t.Errorf("%s imports %s across its layer boundary", path, dependency)
						}
					}
				}
			}
		})
	}
}
