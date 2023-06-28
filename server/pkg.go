package server

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/ije/gox/utils"
	"github.com/ije/gox/valid"
)

type Pkg struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Subpath    string `json:"fullsubmodule"`
	Submodule  string `json:"submodule"`
	FromGithub bool   `json:"fromGithub"`
	FromEsmsh  bool   `json:"fromEsmsh"`
}

func validatePkgPath(pathname string) (pkg Pkg, query string, err error) {
	fromGithub := strings.HasPrefix(pathname, "/gh/") && strings.Count(pathname, "/") >= 3
	if fromGithub {
		pathname = "/@" + pathname[4:]
	}

	pkgName, subpath := splitPkgPath(pathname)
	name, maybeVersion := utils.SplitByLastByte(pkgName, '@')
	if strings.HasPrefix(pkgName, "@") {
		name, maybeVersion = utils.SplitByLastByte(pkgName[1:], '@')
		name = "@" + name
	}
	fromEsmsh := strings.HasPrefix(name, "~") && valid.IsHexString(name[1:])
	if !fromEsmsh && !validatePackageName(name) {
		return Pkg{}, "", fmt.Errorf("invalid package name '%s'", name)
	}

	version, query := utils.SplitByFirstByte(maybeVersion, '&')
	if v, e := url.QueryUnescape(version); e == nil {
		version = v
	}

	/**
	 * Combine subpath with query and parse as a query string.
	 * The last part of the query string will be used as the subpath if it starts with a slash.
	 * If it contains a slash and does not contain equals, the part after the slash will be
	 * used as the subpath.
	 *
	 * This is to cover import_map use cases that contain slashes in the query string embedded
	 * in the path. For the URL `https://esm.sh/example@1.2.3&alias=react:preact/compat` for
	 * example, the subpath will be "compat", which results in wrong query string values.
	 *
	 * After re-combining the query and the subpath and parsing them as a query string, the
	 * alias value is correctly preserved, since that part of the query string contains the
	 * equals sign. The subpath then becomes empty.
	 *
	 * This also allows using relative imports in the import_map for URLs like these. For
	 * example, if we wanted to use the above URL for relative imports, we would have to write
	 * this in the import_map: `https://esm.sh/example@1.2.3&alias=react:preact/compat&/`
	 *
	 * A file request, eg. `https://esm.sh/example@latest&alias=react:preact/compat&/index`,
	 * would result in the last part of the query string (`/index`, the part after the `&`)
	 * being used as the subpath, since it starts with a slash.
	 *
	 * If the last argument of the query string doesn't have a value (and therefore no equals
	 * sign), the part after the first slash will be used as the subpath. For example, the URL
	 * `https://esm.sh/example@1.2.3&pin=v123&dev/index` would result in the subpath being
	 * set to `index`.
	 */
	if query != "" {
		// Combine the query with the subpath.
		query = query + "/" + subpath
		subpath = ""

		parts := strings.Split(query, "&")

		if len(parts) > 1 {
			lastPart := parts[len(parts)-1]

			if strings.HasPrefix(parts[len(parts)-1], "/") {
				// If the last part of the query string starts with a slash, it will be used as
				// the subpath.
				subpath = lastPart[1:]
				query = strings.Join(parts[:len(parts)-1], "&")
			} else if strings.Contains(lastPart, "/") && !strings.Contains(lastPart, "=") {
				// If the last part of the query string contains a slash and no equals sign,
				// the part after the slash will be used as the subpath.
				slashIndex := strings.Index(lastPart, "/")
				subpath = lastPart[slashIndex+1:]
				parts[len(parts)-1] = lastPart[:slashIndex]
				query = strings.Join(parts, "&")
			}
		}
	}

	pkg = Pkg{
		Name:       name,
		Version:    version,
		Subpath:    subpath,
		Submodule:  toModuleName(subpath),
		FromGithub: fromGithub,
		FromEsmsh:  fromEsmsh,
	}

	if fromEsmsh {
		pkg.Version = "0.0.0"
		return
	}

	if fromGithub {
		// strip the leading `@`
		pkg.Name = pkg.Name[1:]
		if (valid.IsHexString(pkg.Version) && len(pkg.Version) >= 10) || regexpFullVersion.MatchString(strings.TrimPrefix(pkg.Version, "v")) {
			return
		}
		var refs []GitRef
		refs, err = listRepoRefs(fmt.Sprintf("https://github.com/%s", pkg.Name))
		if err != nil {
			return
		}
		if pkg.Version == "" {
			for _, ref := range refs {
				if ref.Ref == "HEAD" {
					pkg.Version = ref.Sha[:10]
					return
				}
			}
		} else if strings.HasPrefix(pkg.Version, "semver:") {
			// TODO: support semver
		} else {
			for _, ref := range refs {
				if ref.Ref == "refs/tags/"+pkg.Version || ref.Ref == "refs/heads/"+pkg.Version {
					pkg.Version = ref.Sha[:10]
					return
				}
			}
		}
		err = fmt.Errorf("tag or branch not found")
		return
	}

	// use fixed version
	for prefix, fixedVersion := range fixedPkgVersions {
		if strings.HasPrefix(name+"@"+version, prefix) {
			pkg.Version = fixedVersion
			return
		}
	}

	if regexpFullVersion.MatchString(version) {
		return
	}

	p, _, err := getPackageInfo("", name, version)
	if err == nil {
		pkg.Version = p.Version
	}
	return
}

func (pkg Pkg) Equels(other Pkg) bool {
	return pkg.Name == other.Name && pkg.Version == other.Version && pkg.Submodule == other.Submodule
}

func (pkg Pkg) ImportPath() string {
	if pkg.Submodule != "" {
		return pkg.Name + "/" + pkg.Submodule
	}
	return pkg.Name
}

func (pkg Pkg) VersionName() string {
	s := pkg.Name + "@" + pkg.Version
	if pkg.FromGithub {
		s = "gh/" + s
	}
	return s
}

func (pkg Pkg) String() string {
	s := pkg.VersionName()
	if pkg.Submodule != "" {
		s += "/" + pkg.Submodule
	}
	return s
}

type PathSlice []string

func (a PathSlice) Len() int      { return len(a) }
func (a PathSlice) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a PathSlice) Less(i, j int) bool {
	return len(strings.Split(a[i], "/")) < len(strings.Split(a[j], "/"))
}

// sortable pkg slice
type PkgSlice []Pkg

func (a PkgSlice) Len() int           { return len(a) }
func (a PkgSlice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a PkgSlice) Less(i, j int) bool { return a[i].String() < a[j].String() }

func (a PkgSlice) Has(name string) bool {
	for _, m := range a {
		if m.Name == name {
			return false
		}
	}
	return false
}

func (a PkgSlice) Get(name string) (Pkg, bool) {
	for _, m := range a {
		if m.Name == name {
			return m, true
		}
	}
	return Pkg{}, false
}

func (a PkgSlice) String() string {
	s := make([]string, a.Len())
	for i, m := range a {
		s[i] = m.String()
	}
	return strings.Join(s, ",")
}

func toModuleName(path string) string {
	if path != "" {
		submodule := path
		if strings.HasSuffix(submodule, ".mjs") {
			submodule = strings.TrimSuffix(submodule, ".mjs")
		} else if strings.HasSuffix(submodule, ".cjs") {
			submodule = strings.TrimSuffix(submodule, ".cjs")
		} else {
			submodule = strings.TrimSuffix(submodule, ".js")
		}
		submodule = strings.TrimSuffix(submodule, "/index")
		return submodule
	}
	return ""
}

func splitPkgPath(pathname string) (pkgName string, subpath string) {
	a := strings.Split(strings.TrimPrefix(pathname, "/"), "/")
	pkgName = a[0]
	subpath = strings.Join(a[1:], "/")
	if strings.HasPrefix(pkgName, "@") && len(a) > 1 {
		pkgName = a[0] + "/" + a[1]
		subpath = strings.Join(a[2:], "/")
	}
	return
}
