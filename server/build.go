package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/ije/gox/utils"
	"github.com/postui/postdb"
	"github.com/postui/postdb/q"
)

var targets = map[string]api.Target{
	"deno":   api.ESNext,
	"es2015": api.ES2015,
	"es2016": api.ES2016,
	"es2017": api.ES2017,
	"es2018": api.ES2018,
	"es2019": api.ES2019,
	"es2020": api.ES2020,
}

// todo: use queue instead of lock
var buildLock sync.Mutex

type buildOptions struct {
	config
	pkg    pkg
	deps   pkgSlice
	target string
	isDev  bool
}

type buildResult struct {
	buildID string
	esmeta  *ESMeta
	hasCSS  bool
}

func buildESM(options buildOptions) (ret buildResult, err error) {
	pkg := options.pkg
	target := options.target
	filename := path.Base(pkg.name)
	if pkg.submodule != "" {
		filename = pkg.submodule
	}
	if options.isDev {
		filename += ".development"
	}
	if len(options.deps) > 0 {
		sort.Sort(options.deps)
		target = fmt.Sprintf("deps=%s/%s", strings.ReplaceAll(options.deps.String(), "/", "_"), target)
	}
	buildID := fmt.Sprintf(
		"v%d/%s@%s/%s/%s",
		VERSION,
		pkg.name,
		pkg.version,
		target,
		filename,
	)

	post, err := db.Get(q.Alias(buildID), q.K("esmeta", "css"))
	if err == nil {
		err = json.Unmarshal(post.KV.Get("esmeta"), &ret.esmeta)
		if err != nil {
			_, err = db.Delete(q.Alias(buildID))
			if err != nil {
				return
			}
		}

		if val := post.KV.Get("css"); len(val) == 1 && val[0] == 1 {
			ret.hasCSS = fileExists(path.Join(options.storageDir, "builds", buildID+".css"))
		}

		if fileExists(path.Join(options.storageDir, "builds", buildID+".js")) {
			ret.buildID = buildID
			// has built
			return
		}

		_, err = db.Delete(q.Alias(buildID))
		if err != nil {
			return
		}
	}
	if err != nil && err != postdb.ErrNotFound {
		return
	}

	return build(buildID, options)
}

func build(buildID string, options buildOptions) (ret buildResult, err error) {
	buildLock.Lock()
	defer buildLock.Unlock()

	start := time.Now()
	pkg := options.pkg
	importPath := pkg.ImportPath()
	hasher := sha1.New()
	hasher.Write([]byte(buildID))
	buildDir := path.Join(os.TempDir(), "esm-build-"+hex.EncodeToString(hasher.Sum(nil)))
	ensureDir(buildDir)
	defer os.RemoveAll(buildDir)

	esmeta, err := initBuild(buildDir, pkg)
	if err != nil {
		return
	}

	buf := bytes.NewBuffer(nil)
	exports := []string{}
	hasDefaultExport := false
	env := "production"
	if options.isDev {
		env = "development"
	}
	for _, name := range esmeta.Exports {
		if name == "default" {
			hasDefaultExport = true
		} else if name != "import" {
			exports = append(exports, name)
		}
	}
	if esmeta.Module != "" {
		if len(exports) > 0 {
			fmt.Fprintf(buf, `export {%s} from "%s";%s`, strings.Join(exports, ","), importPath, "\n")
		}
		if hasDefaultExport {
			fmt.Fprintf(buf, `export {default} from "%s";`, importPath)
		}
	} else {
		if len(exports) > 0 {
			fmt.Fprintf(buf, `export {%s,default} from "%s";%s`, strings.Join(exports, ","), importPath, "\n")
		} else {
			fmt.Fprintf(buf, `export {default} from "%s";`, importPath)
		}
	}
	input := &api.StdinOptions{
		Contents:   buf.String(),
		ResolveDir: buildDir,
		Sourcefile: "export.js",
	}
	minify := !options.isDev
	define := map[string]string{
		"__filename":                  fmt.Sprintf(`"https://%s/%s.js"`, options.domain, buildID),
		"__dirname":                   fmt.Sprintf(`"https://%s/%s"`, options.domain, path.Dir(buildID)),
		"process":                     "__process$",
		"Buffer":                      "__Buffer$",
		"setImmediate":                "__setImmediate$",
		"clearImmediate":              "clearTimeout",
		"require.resolve":             "__rResolve$",
		"process.env.NODE_ENV":        fmt.Sprintf(`"%s"`, env),
		"global":                      "__global$",
		"global.process":              "__process$",
		"global.Buffer":               "__Buffer$",
		"global.setImmediate":         "__setImmediate$",
		"global.clearImmediate":       "clearTimeout",
		"global.require.resolve":      "__rResolve$",
		"global.process.env.NODE_ENV": fmt.Sprintf(`"%s"`, env),
	}
	external := newStringSet()
	esmResolverPlugin := api.Plugin{
		Name: "esm-resolver",
		Setup: func(plugin api.PluginBuild) {
			plugin.OnResolve(
				api.OnResolveOptions{Filter: ".*"},
				func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					p := args.Path
					importName := pkg.name
					if pkg.submodule != "" {
						importName += "/" + pkg.submodule
					}
					if p == importName || isFileImportPath(p) {
						return api.OnResolveResult{}, nil
					}
					external.Add(p)
					return api.OnResolveResult{Path: "esm_sh_external://" + p, External: true}, nil
				},
			)
		},
	}
	result := api.Build(api.BuildOptions{
		Stdin:             input,
		Outdir:            "/esbuild",
		Write:             false,
		Bundle:            true,
		Target:            targets[options.target],
		Format:            api.FormatESModule,
		MinifyWhitespace:  minify,
		MinifyIdentifiers: minify,
		MinifySyntax:      minify,
		Define:            define,
		Plugins:           []api.Plugin{esmResolverPlugin},
	})
	if len(result.Errors) > 0 {
		err = errors.New("esbuild: " + result.Errors[0].Text)
		return
	}
	for _, w := range result.Warnings {
		log.Warn(w.Text)
	}

	hasCSS := []byte{0}
	for _, file := range result.OutputFiles {
		outputContent := file.Contents
		if strings.HasSuffix(file.Path, ".js") {
			jsHeader := bytes.NewBufferString(fmt.Sprintf(
				"/* esm.sh - esbuild bundle(%s) %s %s */\n",
				options.pkg.String(),
				strings.ToLower(options.target),
				env,
			))
			eol := "\n"
			if !options.isDev {
				eol = ""
			}

			// replace external imports/requires
			for _, name := range external.Values() {
				var importPath string
				if options.target == "deno" {
					_, yes := denoStdNodeModules[name]
					if yes {
						importPath = fmt.Sprintf("/v%d/_deno_std_node_%s.js", VERSION, name)
					}
				}
				if name == "buffer" {
					importPath = fmt.Sprintf("/v%d/_node_buffer.js", VERSION)
				}
				if importPath == "" {
					polyfill, ok := polyfilledBuiltInNodeModules[name]
					if ok {
						p, submodule, e := nodeEnv.getPackageInfo(polyfill, "latest")
						if e == nil {
							filename := path.Base(p.Name)
							if submodule != "" {
								filename = submodule
							}
							if options.isDev {
								filename += ".development"
							}
							importPath = fmt.Sprintf(
								"/v%d/%s@%s/%s/%s.js",
								VERSION,
								p.Name,
								p.Version,
								options.target,
								filename,
							)
						} else {
							err = e
							return
						}
					} else {
						_, err := embedFS.Open(fmt.Sprintf("polyfills/node_%s.js", name))
						if err == nil {
							importPath = fmt.Sprintf("/v%d/_node_%s.js", VERSION, name)
						}
					}
				}
				if importPath == "" {
					packageFile := path.Join(buildDir, "node_modules", name, "package.json")
					if fileExists(packageFile) {
						var p NpmPackage
						if utils.ParseJSONFile(packageFile, &p) == nil {
							suffix := ".js"
							if options.isDev {
								suffix = ".development.js"
							}
							importPath = fmt.Sprintf(
								"/v%d/%s@%s/%s/%s%s",
								VERSION,
								p.Name,
								p.Version,
								options.target,
								path.Base(p.Name),
								suffix,
							)
						}
					}
				}
				if importPath == "" {
					version := "latest"
					for _, dep := range options.deps {
						if name == dep.name {
							version = dep.version
							break
						}
					}
					if version == "latest" {
						for n, v := range esmeta.Dependencies {
							if name == n {
								version = v
								break
							}
						}
					}
					if version == "latest" {
						for n, v := range esmeta.PeerDependencies {
							if name == n {
								version = v
								break
							}
						}
					}
					p, submodule, e := nodeEnv.getPackageInfo(name, version)
					if e == nil {
						filename := path.Base(p.Name)
						if submodule != "" {
							filename = submodule
						}
						if options.isDev {
							filename += ".development"
						}
						importPath = fmt.Sprintf(
							"/v%d/%s@%s/%s/%s.js",
							VERSION,
							p.Name,
							p.Version,
							options.target,
							filename,
						)
					}
				}
				if importPath == "" {
					importPath = fmt.Sprintf("/_error.js?type=resolve&name=%s", name)
				}
				buf := bytes.NewBuffer(nil)
				identifier := identify(name)
				slice := bytes.Split(outputContent, []byte(fmt.Sprintf("\"esm_sh_external://%s\"", name)))
				commonjs := false
				commonjsImported := false
				for i, p := range slice {
					if commonjs {
						p = bytes.TrimPrefix(p, []byte{')'})
					}
					commonjs = bytes.HasSuffix(p, []byte("require("))
					if commonjs {
						p = bytes.TrimSuffix(p, []byte("require("))
						if !commonjsImported {
							fmt.Fprintf(jsHeader, `import __%s$ from "%s";%s`, identifier, importPath, eol)
							commonjsImported = true
						}
					}
					buf.Write(p)
					if i < len(slice)-1 {
						if commonjs {
							buf.WriteString(fmt.Sprintf("__%s$", identifier))
						} else {
							buf.WriteString(fmt.Sprintf("\"%s\"", importPath))
						}
					}
				}
				outputContent = buf.Bytes()
			}

			// add nodejs/deno compatibility
			if bytes.Contains(outputContent, []byte("__process$")) {
				fmt.Fprintf(jsHeader, `import __process$ from "/v%d/_node_process.js";%s__process$.env.NODE_ENV="%s";%s`, VERSION, eol, env, eol)
			}
			if bytes.Contains(outputContent, []byte("__Buffer$")) {
				fmt.Fprintf(jsHeader, `import { Buffer as __Buffer$ } from "/v%d/_node_buffer.js";%s`, VERSION, eol)
			}
			if bytes.Contains(outputContent, []byte("__global$")) {
				fmt.Fprintf(jsHeader, `var __global$ = window;%s`, eol)
			}
			if bytes.Contains(outputContent, []byte("__setImmediate$")) {
				fmt.Fprintf(jsHeader, `var __setImmediate$ = (cb, args) => setTimeout(cb, 0, ...args);%s`, eol)
			}
			if bytes.Contains(outputContent, []byte("__rResolve$")) {
				fmt.Fprintf(jsHeader, `var __rResolve$ = p => p;%s`, eol)
			}

			saveFilePath := path.Join(options.storageDir, "builds", buildID+".js")
			ensureDir(path.Dir(saveFilePath))

			var file *os.File
			file, err = os.Create(saveFilePath)
			if err != nil {
				return
			}
			defer file.Close()

			_, err = io.Copy(file, jsHeader)
			if err != nil {
				return
			}

			_, err = io.Copy(file, bytes.NewReader(outputContent))
			if err != nil {
				return
			}
		} else if strings.HasSuffix(file.Path, ".css") {
			saveFilePath := path.Join(options.storageDir, "builds", buildID+".css")
			ensureDir(path.Dir(saveFilePath))
			file, e := os.Create(saveFilePath)
			if e != nil {
				err = e
				return
			}
			defer file.Close()

			_, err = io.Copy(file, bytes.NewReader(outputContent))
			if err != nil {
				return
			}
			hasCSS = []byte{1}
		}
	}

	log.Debugf("esbuild %s %s %s in %v", options.pkg.String(), options.target, env, time.Now().Sub(start))

	err = handleDTS(buildDir, esmeta, options)
	if err != nil {
		return
	}

	_, err = db.Put(
		q.Alias(buildID),
		q.KV{
			"esmeta": utils.MustEncodeJSON(esmeta),
			"css":    hasCSS,
		},
	)
	if err != nil {
		return
	}

	ret.buildID = buildID
	ret.esmeta = esmeta
	ret.hasCSS = hasCSS[0] == 1
	return
}

func initBuild(buildDir string, pkg pkg) (esmeta *ESMeta, err error) {
	var p NpmPackage
	p, _, err = nodeEnv.getPackageInfo(pkg.name, pkg.version)
	if err != nil {
		return
	}

	esmeta = &ESMeta{
		NpmPackage: &p,
	}
	installList := []string{
		fmt.Sprintf("%s@%s", pkg.name, pkg.version),
	}
	pkgDir := path.Join(buildDir, "node_modules", esmeta.Name)
	if esmeta.Types == "" && esmeta.Typings == "" && !strings.HasPrefix(pkg.name, "@") {
		var info NpmPackage
		info, _, err = nodeEnv.getPackageInfo("@types/"+pkg.name, "latest")
		if err == nil {
			if info.Types != "" || info.Typings != "" || info.Main != "" {
				installList = append(installList, fmt.Sprintf("%s@%s", info.Name, info.Version))
			}
		} else if err.Error() != fmt.Sprintf("npm: package '@types/%s' not found", pkg.name) {
			return
		}
	}
	if esmeta.Module == "" && esmeta.Type == "module" {
		esmeta.Module = esmeta.Main
	}
	if esmeta.Module == "" && esmeta.DefinedExports != nil {
		v, ok := esmeta.DefinedExports.(map[string]interface{})
		if ok {
			m, ok := v["import"]
			if ok {
				s, ok := m.(string)
				if ok && s != "" {
					esmeta.Module = s
				}
			}
		}
	}
	if pkg.submodule != "" {
		esmeta.Main = pkg.submodule
		esmeta.Module = ""
		esmeta.Types = ""
		esmeta.Typings = ""
	}

	err = yarnAdd(buildDir, installList...)
	if err != nil {
		return
	}

	if pkg.submodule != "" {
		if fileExists(path.Join(pkgDir, pkg.submodule, "package.json")) {
			var p NpmPackage
			err = utils.ParseJSONFile(path.Join(pkgDir, pkg.submodule, "package.json"), &p)
			if err != nil {
				return
			}
			if p.Main != "" {
				esmeta.Main = path.Join(pkg.submodule, p.Main)
			}
			if p.Module != "" {
				esmeta.Module = path.Join(pkg.submodule, p.Module)
			} else if esmeta.Type == "module" && p.Main != "" {
				esmeta.Module = path.Join(pkg.submodule, p.Main)
			}
			if p.Types != "" {
				esmeta.Types = path.Join(pkg.submodule, p.Types)
			}
			if p.Typings != "" {
				esmeta.Typings = path.Join(pkg.submodule, p.Typings)
			}
		} else {
			exports, esm, e := parseESModuleExports(buildDir, path.Join(esmeta.Name, pkg.submodule))
			if e != nil {
				err = e
				return
			}
			if esm {
				esmeta.Module = pkg.submodule
				esmeta.Exports = exports
			}
		}
	}

	if esmeta.Module != "" {
		exports, esm, e := parseESModuleExports(buildDir, path.Join(esmeta.Name, esmeta.Module))
		if e != nil {
			err = e
			return
		}
		if esm {
			esmeta.Exports = exports

		} else {
			// fake module
			esmeta.Module = ""
		}
	}

	if esmeta.Module == "" {
		ret, e := parseCJSModuleExports(buildDir, pkg.ImportPath())
		if e != nil {
			err = e
			return
		}
		esmeta.Exports = ret.Exports
	}
	return
}

func handleDTS(buildDir string, esmeta *ESMeta, options buildOptions) (err error) {
	start := time.Now()
	pkg := options.pkg
	nodeModulesDir := path.Join(buildDir, "node_modules")
	nv := fmt.Sprintf("%s@%s", esmeta.Name, esmeta.Version)

	var types string
	if esmeta.Types != "" || esmeta.Typings != "" {
		types = getTypesPath(nodeModulesDir, *esmeta.NpmPackage, "")
	} else if pkg.submodule == "" {
		if fileExists(path.Join(nodeModulesDir, pkg.name, "index.d.ts")) {
			types = fmt.Sprintf("%s/%s", nv, "index.d.ts")
		} else if !strings.HasPrefix(pkg.name, "@") {
			var info NpmPackage
			err = utils.ParseJSONFile(path.Join(nodeModulesDir, "@types", pkg.name, "package.json"), &info)
			if err == nil {
				types = getTypesPath(nodeModulesDir, info, "")
			} else if !os.IsNotExist(err) {
				return
			}
		}
	} else {
		if fileExists(path.Join(nodeModulesDir, pkg.name, pkg.submodule, "index.d.ts")) {
			types = fmt.Sprintf("%s/%s", nv, path.Join(pkg.submodule, "index.d.ts"))
		} else if fileExists(path.Join(nodeModulesDir, pkg.name, ensureExt(pkg.submodule, ".d.ts"))) {
			types = fmt.Sprintf("%s/%s", nv, ensureExt(pkg.submodule, ".d.ts"))
		} else if fileExists(path.Join(nodeModulesDir, "@types", pkg.name, pkg.submodule, "index.d.ts")) {
			types = fmt.Sprintf("@types/%s/%s", nv, path.Join(pkg.submodule, "index.d.ts"))
		} else if fileExists(path.Join(nodeModulesDir, "@types", pkg.name, ensureExt(pkg.submodule, ".d.ts"))) {
			types = fmt.Sprintf("@types/%s/%s", nv, ensureExt(pkg.submodule, ".d.ts"))
		}
	}
	if types != "" {
		err = copyDTS(
			options.config,
			nodeModulesDir,
			types,
		)
		if err != nil {
			err = fmt.Errorf("copyDTS(%s): %v", types, err)
			return
		}
		esmeta.Dts = "/" + types
		log.Debug("copy dts in", time.Now().Sub(start))
	}

	return
}
