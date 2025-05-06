/*
Copyright © 2025 George <george@betterde.com>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/betterde/gonew/internal/edit"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	"go/parser"
	"go/token"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"gopkg.in/yaml.v3"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// initCmd represents the init command
var initCmd = &cobra.Command{
	Use:   "init <src> [dst]",
	Run:   initProject,
	Args:  cobra.MinimumNArgs(1),
	Short: "Initialize a new project using a template",
}

var (
	src string
	dst string
)

func init() {
	rootCmd.AddCommand(initCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// initCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// initCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

func initProject(cmd *cobra.Command, args []string) {
	if len(args) < 1 || len(args) > 3 {
		err := cmd.Usage()
		if err != nil {
			return
		}
	}

	src = args[0]
	ver := src
	if !strings.Contains(ver, "@") {
		ver += "@latest"
	}

	src, _, _ = strings.Cut(src, "@")
	if err := module.CheckPath(src); err != nil {
		log.Fatalf("invalid source module name: %v", err)
	}

	dst = src
	if len(args) >= 2 {
		dst = args[1]
		if err := module.CheckPath(dst); err != nil {
			log.Fatalf("invalid destination module name: %v", err)
		}
	}

	var dir string
	if len(args) == 3 {
		dir = args[2]
	} else {
		dir = "." + string(filepath.Separator) + path.Base(dst)
	}

	// Dir must not exist or must be an empty directory.
	de, err := os.ReadDir(dir)
	if err == nil && len(de) > 0 {
		log.Fatalf("target directory %s exists and is non-empty", dir)
	}
	needMkdir := err != nil

	var stdout, stderr bytes.Buffer
	command := exec.Command("go", "mod", "download", "-json", ver)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err = command.Run(); err != nil {
		log.Fatalf("go mod download -json %s: %v\n%s%s", ver, err, stderr.Bytes(), stdout.Bytes())
	}

	var info struct {
		Dir string
	}
	if err = json.Unmarshal(stdout.Bytes(), &info); err != nil {
		log.Fatalf("go mod download -json %s: invalid JSON output: %v\n%s%s", ver, err, stderr.Bytes(), stdout.Bytes())
	}

	if needMkdir {
		if err := os.MkdirAll(dir, 0777); err != nil {
			log.Fatalf("mkdir error: %s", err)
		}
	}

	// Copy from module cache into new directory, making edits as needed.
	err = filepath.WalkDir(info.Dir, func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Fatal(err)
		}
		rel, err := filepath.Rel(info.Dir, src)
		if err != nil {
			log.Fatal(err)
		}
		dstPath := filepath.Join(dir, rel)
		if d.IsDir() {
			if err := os.MkdirAll(dstPath, 0777); err != nil {
				log.Fatal(err)
			}
			return nil
		}

		data, err := os.ReadFile(src)
		if err != nil {
			log.Fatal(err)
		}

		isRoot := !strings.Contains(rel, string(filepath.Separator))
		if strings.HasSuffix(rel, ".go") {
			data = fixGo(data, rel, src, dst, isRoot)
		}
		if rel == "go.mod" {
			data = fixGoMod(data, dst)
		}

		if err := os.WriteFile(dstPath, data, 0666); err != nil {
			log.Fatal(err)
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	templateFile := filepath.Join(dir, "template.yaml")
	prompts, err := readConfig(templateFile)
	if err != nil {
		log.Fatal(err)
	}

	inputs, err := runPrompts(prompts)
	if err != nil {
		log.Fatal(err)
	}

	err = replaceVars(dir, inputs)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("initialized %s in %s", dst, dir)
}

// fixGo rewrites the Go source in data to replace srcMod with dstMod.
// isRoot indicates whether the file is in the root directory of the module,
// in which case we also update the package name.
func fixGo(data []byte, file string, srcMod, dstMod string, isRoot bool) []byte {
	fileSet := token.NewFileSet()
	f, err := parser.ParseFile(fileSet, file, data, parser.ImportsOnly)
	if err != nil {
		log.Fatalf("parsing source module:\n%s", err)
	}

	buf := edit.NewBuffer(data)
	at := func(p token.Pos) int {
		return fileSet.File(p).Offset(p)
	}

	srcName := path.Base(srcMod)
	dstName := path.Base(dstMod)
	if isRoot {
		if name := f.Name.Name; name == srcName || name == srcName+"_test" {
			dname := dstName + strings.TrimPrefix(name, srcName)
			if !token.IsIdentifier(dname) {
				log.Fatalf("%s: cannot rename package %s to package %s: invalid package name", file, name, dname)
			}
			buf.Replace(at(f.Name.Pos()), at(f.Name.End()), dname)
		}
	}

	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if path == srcMod {
			if srcName != dstName && spec.Name == nil {
				// Add package rename because source code uses original name.
				// The renaming looks strange, but template authors are unlikely to
				// create a template where the root package is imported by packages
				// in subdirectories, and the renaming at least keeps the code working.
				// A more sophisticated approach would be to rename the uses of
				// the package identifier in the file too, but then you have to worry about
				// name collisions, and given how unlikely this is, it doesn't seem worth
				// trying to clean up the file that way.
				buf.Insert(at(spec.Path.Pos()), srcName+" ")
			}
			// Change import path to dstMod
			buf.Replace(at(spec.Path.Pos()), at(spec.Path.End()), strconv.Quote(dstMod))
		}
		if strings.HasPrefix(path, srcMod+"/") {
			// Change import path to begin with dstMod
			buf.Replace(at(spec.Path.Pos()), at(spec.Path.End()), strconv.Quote(strings.Replace(path, srcMod, dstMod, 1)))
		}
	}
	return buf.Bytes()
}

// fixGoMod rewrites the go.mod content in data to replace srcMod with dstMod
// in the module path.
func fixGoMod(data []byte, dstMod string) []byte {
	file, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		log.Fatalf("parsing source module:\n%s", err)
	}
	err = file.AddModuleStmt(dstMod)
	if err != nil {
		log.Fatalf("add module stmt:\n%s", err)
	}
	format, err := file.Format()
	if err != nil {
		return data
	}
	return format
}

// readConfig Reading YAML configuration files
func readConfig(filename string) (map[string]string, error) {
	var config map[string]string
	data, err := os.ReadFile(filename)
	if err != nil {
		return config, err
	}
	err = yaml.Unmarshal(data, &config)
	return config, err
}

// runPrompts Run interactive prompts based on configuration
func runPrompts(config map[string]string) (map[string]string, error) {
	answers := make(map[string]string)

	for key, desc := range config {
		prompt := promptui.Prompt{
			Label: desc,
			Validate: func(input string) error {
				if len(input) == 0 {
					return fmt.Errorf(desc)
				}
				return nil
			},
		}

		name, err := prompt.Run()
		if err != nil {
			return nil, err
		}
		answers[key] = name
	}

	return answers, nil
}

func replaceVars(dir string, inputs map[string]string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			relPath, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			return generateFile(inputs, relPath, string(content), dir)
		}
		return nil
	})
}

// generateFile creates a single file from a template
func generateFile(data map[string]string, fileName, content, projectDir string) error {
	// Parse the template
	tmpl, err := template.New(fileName).Parse(content)
	if err != nil {
		return fmt.Errorf("error parsing template %s: %v", fileName, err)
	}

	// Create the output file
	filePath := filepath.Join(projectDir, fileName)
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return fmt.Errorf("error creating directories for %s: %v", fileName, err)
	}

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("error creating file %s: %v", fileName, err)
	}
	defer file.Close()

	// Execute the template and write to file
	if err := tmpl.Execute(file, data); err != nil {
		return fmt.Errorf("error executing template %s: %v", fileName, err)
	}

	return nil
}
