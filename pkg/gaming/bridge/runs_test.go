// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bridge

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bridge runs nothing.
//
// Its host holds the keys. A path from a connected game to starting a process
// on that host would make the entire policy model - caps, approval, the
// operator's password - a thing to be walked around rather than through.
func TestTheBridgeRunsNothing(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return nil // generated or unparseable files are not what this is about
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "os/exec" {
				t.Errorf("%s runs processes: a bridge that can start programs is a bridge whose "+
					"spending limits can be stepped around", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the bridge: %v", err)
	}
}
