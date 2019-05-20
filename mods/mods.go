// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mods

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gohugoio/hugo/common/hugio"

	"github.com/pkg/errors"
	"github.com/spf13/afero"
)

var (
	fileSeparator = string(os.PathSeparator)
)

const hugoModProxyEnvKey = "HUGO_MODPROXY"

const (
	goBinaryStatusOK goBinaryStatus = iota
	goBinaryStatusNotFound
	goBinaryStatusTooOld
)

// The "vendor" dir is reserved for Go Modules.
const vendord = "_vendor"

// These are the folders we consider to be part of a module when we vendor
// it.
// TODO(bep) mod configurable...? regexp?
var dirnames = map[string]bool{
	"archetypes": true,
	"assets":     true,
	"data":       true,
	"i18n":       true,
	"layouts":    true,
	"resources":  true,
	"static":     true,
}

const (
	goModFilename = "go.mod"
	goSumFilename = "go.sum"
)

func NewClient(fs afero.Fs, workingDir, themesDir string, imports []string) *Client {

	n := filepath.Join(workingDir, goModFilename)
	goModEnabled, _ := afero.Exists(fs, n)
	var goModFilename string
	if goModEnabled {
		goModFilename = n
	}

	env := os.Environ()
	setEnvVars(&env, "PWD", workingDir, "GOPROXY", getGoProxy())

	return &Client{
		fs:                fs,
		workingDir:        workingDir,
		themesDir:         themesDir,
		imports:           imports,
		environ:           env,
		GoModulesFilename: goModFilename}
}

// Client contains most of the API provided by this package.
type Client struct {
	fs afero.Fs

	// Absolute path to the project dir.
	workingDir string

	// Absolute path to the project's themes dir.
	themesDir string

	// The top level module imports.
	imports []string

	// Environment variables used in "go get" etc.
	environ []string

	// Set when Go modules are initialized in the current repo, that is:
	// a go.mod file exists.
	GoModulesFilename string

	// Set if we get a exec.ErrNotFound when running Go, which is most likely
	// due to being run on a system without Go installed. We record it here
	// so we can give an instructional error at the end if module/theme
	// resolution fails.
	goBinaryStatus goBinaryStatus
}

// TODO(bep) mod probably filter this against imports? Also check replace.
// TODO(bep) merge with _vendor + /theme
func (m *Client) Graph(w io.Writer) error {
	mc, err := m.Collect()
	if err != nil {
		return err
	}
	for _, module := range mc.Modules {
		// sourcegraph.com/sourcegraph/go-diff@v0.5.0 github.com/gogo/protobuf@v1.1.1
		fmt.Fprintln(w, pathVersion(module.Owner())+" "+pathVersion(module))

	}

	return nil
}

func (m *Client) Tidy() error {
	tc, err := m.Collect()
	if err != nil {
		return err
	}

	isGoMod := make(map[string]bool)
	for _, m := range tc.Modules {
		if m.IsGoMod() {
			// Matching the format in go.mod
			isGoMod[m.Path()+" "+m.Version()] = true
		}
	}

	if err := m.rewriteGoMod(goModFilename, isGoMod); err != nil {
		return err
	}

	// Now go.mod contains only in-use modules. The go.sum file will
	// contain the entire dependency graph, so we need to check against that.
	// TODO(bep) check if needed
	/*graph, err := m.graphStr()
	if err != nil {
		return err
	}

	isGoMod = make(map[string]bool)
	graphItems := strings.Split(graph, "\n")
	for _, item := range graphItems {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		modver := strings.Replace(strings.Fields(item)[1], "@", " ", 1)
		isGoMod[modver] = true
	}*/

	if err := m.rewriteGoMod(goSumFilename, isGoMod); err != nil {
		return err
	}

	return nil
}

func (m *Client) Get(args ...string) error {
	if err := m.runGo(context.Background(), os.Stdout, append([]string{"get"}, args...)...); err != nil {
		errors.Wrapf(err, "failed to get %q", args)
	}
	return nil
}

func (m *Client) Init(path string) error {

	err := m.runGo(context.Background(), os.Stdout, "mod", "init", path)
	if err != nil {
		return errors.Wrap(err, "failed to init modules")
	}

	m.GoModulesFilename = filepath.Join(m.workingDir, goModFilename)

	return nil
}

func (m *Client) IsProbablyModule(path string) bool {
	// Very simple for now.
	return m.GoModulesFilename != "" && strings.Contains(path, "/")
}

// Like Go, Hugo supports writing the dependencies to a /vendor folder.
// Unlike Go, we support it for any level.
// We, by defaults, use the /vendor folder first, if found. To disable,
// run with
//    hugo --no-vendor TODO(bep) also on hugo mod
//
// Given a module tree, Hugo will pick the first module for a given path,
// meaning that if the top-level module is vendored, that will be the full
// set of dependencies.
func (c *Client) Vendor() error {
	vendorDir := filepath.Join(c.workingDir, vendord)
	if err := c.rmVendorDir(vendorDir); err != nil {
		return err
	}

	// Write the modules list to modules.txt.
	//
	// On the form:
	//
	// # github.com/alecthomas/chroma v0.6.3
	//
	// This is how "go mod vendor" does it. Go also lists
	// the packages below it, but that is currently not applicable to us.
	//
	var modulesContent bytes.Buffer

	tc, err := c.Collect()
	if err != nil {
		return err
	}

	for _, t := range tc.Modules {
		if !t.IsGoMod() {
			// TODO(bep) mod consider /themes
			continue
		}

		fmt.Fprintln(&modulesContent, "# "+t.Path()+" "+t.Version())

		dir := t.Dir()

		shouldCopy := func(filename string) bool {
			base := filepath.Base(strings.TrimPrefix(filename, dir))
			// Only vendor the root files + the predefined set of  folders.
			// TODO(bep) rework this whitelist idea.
			return base != "_vendor" //dirnames[base]
		}

		if err := hugio.CopyDir(c.fs, dir, filepath.Join(vendorDir, t.Path()), shouldCopy); err != nil {
			return errors.Wrap(err, "failed to copy module to vendor dir")
		}
	}

	if modulesContent.Len() > 0 {
		if err := afero.WriteFile(c.fs, filepath.Join(vendorDir, vendorModulesFilename), modulesContent.Bytes(), 0666); err != nil {
			return err
		}
	}

	return nil
}

func (m *Client) listGoMods() (goModules, error) {
	if m.GoModulesFilename == "" {
		return nil, nil
	}
	///
	// TODO(bep) mod check permissions
	// TODO(bep) mod clear cache
	// TODO(bep) mount at all of partials/ partials/v1  partials/v2 or something.
	// TODO(bep) rm: public/images/logos/made-with-bulma.png: Permission denied
	// TODO(bep) watch pkg cache?
	//  0555 directories
	// TODO(bep) mod hugo mod init
	// GO111MODULE=on
	//

	// TODO(bep) mod --no-vendor flag (also on hugo)
	// TODO(bep) mod hugo mod vendor: --no-local
	// GOCACHE

	out := ioutil.Discard
	err := m.runGo(context.Background(), out, "mod", "download")
	if err != nil {
		return nil, errors.Wrap(err, "failed to download modules")
	}

	b := &bytes.Buffer{}
	err = m.runGo(context.Background(), b, "list", "-m", "-json", "all")
	if err != nil {
		return nil, errors.Wrap(err, "failed to list modules")
	}

	var modules goModules

	dec := json.NewDecoder(b)
	for {
		m := &goModule{}
		if err := dec.Decode(m); err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.Wrap(err, "failed to decode modules list")
		}

		modules = append(modules, m)
	}

	return modules, err

}

func (m *Client) rewriteGoMod(name string, isGoMod map[string]bool) error {
	data, err := m.rewriteGoModRewrite(name, isGoMod)
	if err != nil {
		return err
	}
	if data != nil {
		// Rewrite the file.
		if err := afero.WriteFile(m.fs, filepath.Join(m.workingDir, name), data, 0666); err != nil {
			return err
		}
	}

	return nil
}

func (m *Client) rewriteGoModRewrite(name string, isGoMod map[string]bool) ([]byte, error) {
	if name == goModFilename && m.GoModulesFilename == "" {
		// Already checked.
		return nil, nil
	}

	isModLine := func(s string) bool {
		return true
	}

	if name == goModFilename {
		isModLine = func(s string) bool {
			return strings.HasPrefix(s, "mod require") || strings.HasPrefix(s, "\t")
		}
	}

	b := &bytes.Buffer{}
	f, err := m.fs.Open(filepath.Join(m.workingDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			// It's been deleted.
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var dirty bool

	for scanner.Scan() {
		line := scanner.Text()
		var doWrite bool

		if isModLine(line) {
			modname := strings.TrimSpace(line)
			if modname == "" {
				doWrite = true
			} else {
				// TODO(bep) mod: mod require
				parts := strings.Fields(modname)
				if len(parts) >= 2 {
					// [module path] [version]/go.mod
					modname, modver := parts[0], parts[1]
					modver = strings.TrimSuffix(modver, "/"+goModFilename)
					doWrite = isGoMod[modname+" "+modver]
				}
			}
		} else {
			doWrite = true
		}

		if doWrite {
			fmt.Fprintln(b, line)
		} else {
			dirty = true
		}
	}

	if !dirty {
		// Nothing changed
		return nil, nil
	}

	return b.Bytes(), nil

}

func (c *Client) rmVendorDir(vendorDir string) error {
	modulestxt := filepath.Join(vendorDir, vendorModulesFilename)

	if _, err := c.fs.Stat(vendorDir); err != nil {
		return nil
	}

	_, err := c.fs.Stat(modulestxt)
	if err != nil {
		// If we have a _vendor dir without modules.txt it sounds like
		// a _vendor dir created by others.
		return errors.New("found _vendor dir without modules.txt, skip delete")
	}

	return c.fs.RemoveAll(vendorDir)
}

func (m *Client) runGo(
	ctx context.Context,
	stdout io.Writer,
	args ...string) error {

	if m.goBinaryStatus != 0 {
		return nil
	}

	stderr := new(bytes.Buffer)
	cmd := exec.CommandContext(ctx, "go", args...)

	cmd.Env = m.environ
	cmd.Dir = m.workingDir
	cmd.Stdout = stdout
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)

	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.Error); ok && ee.Err == exec.ErrNotFound {
			m.goBinaryStatus = goBinaryStatusNotFound
			return nil
		}

		_, ok := err.(*exec.ExitError)
		if !ok {
			return errors.Errorf("failed to execute 'go %v': %s %T", args, err, err)
		}

		// Too old Go version
		if strings.Contains(stderr.String(), "flag provided but not defined") {
			m.goBinaryStatus = goBinaryStatusTooOld
			return nil
		}

		return errors.Errorf("go command failed: %s", stderr)

	}

	return nil
}

type ModuleError struct {
	Err string // the error itself
}

type goBinaryStatus int

type goModule struct {
	Path     string       // module path
	Version  string       // module version
	Versions []string     // available module versions (with -versions)
	Replace  *goModule    // replaced by this module
	Time     *time.Time   // time version was created
	Update   *goModule    // available update, if any (with -u)
	Main     bool         // is this the main module?
	Indirect bool         // is this module only an indirect dependency of main module?
	Dir      string       // directory holding files for this module, if any
	GoMod    string       // path to go.mod file for this module, if any
	Error    *ModuleError // error loading module
}

type goModules []*goModule

func (modules goModules) GetByPath(p string) *goModule {
	if modules == nil {
		return nil
	}

	for _, m := range modules {
		if strings.EqualFold(p, m.Path) {
			return m
		}
	}

	return nil
}

func (modules goModules) GetMain() *goModule {
	for _, m := range modules {
		if m.Main {
			return m
		}
	}

	return nil
}

func setEnvVar(vars *[]string, key, value string) {
	for i := range *vars {
		if strings.HasPrefix((*vars)[i], key+"=") {
			(*vars)[i] = key + "=" + value
			return
		}
	}
	// New var.
	*vars = append(*vars, key+"="+value)
}

func setEnvVars(oldVars *[]string, keyValues ...string) {
	for i := 0; i < len(keyValues); i += 2 {
		setEnvVar(oldVars, keyValues[i], keyValues[i+1])
	}
}

func getGoProxy() string {
	if hp := os.Getenv(hugoModProxyEnvKey); hp != "" {
		return hp
	}

	// Defaeult to direct, which means "git clone" and similar. We
	// will investigate proxy settings in more depth later.
	// See https://github.com/golang/go/issues/26334
	return "direct"
}

func pathVersion(m Module) string {
	versionStr := m.Version()
	if m.Vendor() {
		versionStr = "vendor"
	}
	if versionStr == "" {
		return m.Path()
	}
	return fmt.Sprintf("%s@%s", m.Path(), versionStr)
}