// Package js menjalankan script tes (JS/TS) per virtual user dan menyediakan modul k6.
package js

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

// Compile membundel script (beserta import lokalnya, TS maupun JS) lalu mengompilasinya
// satu kali. Hasilnya dipakai bersama oleh semua VU.
func Compile(path string) (*goja.Program, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, "", fmt.Errorf("file script %q tidak ditemukan", path)
	}
	dir := filepath.Dir(abs)
	res := api.Build(api.BuildOptions{
		EntryPoints:   []string{abs},
		Outfile:       filepath.Join(dir, "__serbu_bundle__.js"),
		Bundle:        true,
		Write:         false,
		Format:        api.FormatCommonJS,
		Platform:      api.PlatformNeutral,
		MainFields:    []string{"module", "main"},
		Target:        api.ES2017,
		Sourcemap:     api.SourceMapInline,
		KeepNames:     true,
		LegalComments: api.LegalCommentsNone,
		Charset:       api.CharsetUTF8,
		AbsWorkingDir: dir,
		LogLevel:      api.LogLevelSilent,
		External:      []string{"k6", "k6/*", "serbu", "serbu/*", "https://*", "http://*"},
		Loader: map[string]api.Loader{
			".json": api.LoaderJSON,
			".txt":  api.LoaderText,
			".csv":  api.LoaderText,
		},
	})
	if len(res.Errors) > 0 {
		var b strings.Builder
		for _, e := range res.Errors {
			if e.Location != nil {
				fmt.Fprintf(&b, "%s:%d:%d: %s\n    %s\n", e.Location.File, e.Location.Line, e.Location.Column+1, e.Text, strings.TrimSpace(e.Location.LineText))
			} else {
				fmt.Fprintf(&b, "%s\n", e.Text)
			}
		}
		return nil, "", fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}
	var code string
	for _, f := range res.OutputFiles {
		if strings.HasSuffix(f.Path, ".js") {
			code = string(f.Contents)
		}
	}
	prg, err := goja.Compile(abs, code, false)
	if err != nil {
		return nil, "", err
	}
	return prg, dir, nil
}
