// Copyright 2013 Gary Burd
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

// Package lintapp implements the go-lint.appspot.com server.
package lintapp

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"

	"github.com/garyburd/gosrc"
	"github.com/golang/lint"
)

func init() {
	http.Handle("/", handlerFunc(serveRoot))
	http.Handle("/-/bot", handlerFunc(serveBot))
	http.Handle("/-/refresh", handlerFunc(serveRefresh))
}

var (
	contactEmail    = "unknown@example.com"
	homeTemplate    = parseTemplate("common.html", "index.html")
	packageTemplate = parseTemplate("common.html", "package.html")
	errorTemplate   = parseTemplate("common.html", "error.html")
	templateFuncs   = template.FuncMap{
		"timeago":      timeagoFn,
		"contactEmail": contactEmailFn,
	}
)

func parseTemplate(fnames ...string) *template.Template {
	paths := make([]string, len(fnames))
	for i := range fnames {
		paths[i] = filepath.Join("assets/templates", fnames[i])
	}
	t, err := template.New("").Funcs(templateFuncs).ParseFiles(paths...)
	if err != nil {
		panic(err)
	}
	t = t.Lookup("ROOT")
	if t == nil {
		panic(fmt.Sprintf("ROOT template not found in %v", fnames))
	}
	return t
}

func contactEmailFn() string {
	return contactEmail
}

func timeagoFn(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < 2*time.Second:
		return "one second ago"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", d/time.Second)
	case d < 2*time.Minute:
		return "one minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", d/time.Minute)
	case d < 2*time.Hour:
		return "one hour ago"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", d/time.Hour)
	default:
		return fmt.Sprintf("%d days ago", d/(time.Hour*24))
	}
}

func writeResponse(w http.ResponseWriter, status int, t *template.Template, v interface{}) error {
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

func writeErrorResponse(w http.ResponseWriter, status int) error {
	return writeResponse(w, status, errorTemplate, http.StatusText(status))
}

const version = 1

type storePackage struct {
	Data    []byte
	Version int
}

type lintPackage struct {
	Files   []*lintFile
	Path    string
	Updated time.Time
	LineFmt string
	URL     string
}

type lintFile struct {
	Name     string
	Problems []*lintProblem
	URL      string
}

type lintProblem struct {
	Line       int
	Text       string
	LineText   string
	Confidence float64
}

func putPackage(c appengine.Context, importPath string, pkg *lintPackage) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(pkg); err != nil {
		return err
	}
	_, err := datastore.Put(c,
		datastore.NewKey(c, "Pacakge", importPath, 0, nil),
		&storePackage{Data: buf.Bytes(), Version: version})
	return err
}

func getPackage(c appengine.Context, importPath string) (*lintPackage, error) {
	var spkg storePackage
	if err := datastore.Get(c, datastore.NewKey(c, "Pacakge", importPath, 0, nil), &spkg); err != nil {
		if err == datastore.ErrNoSuchEntity {
			err = nil
		}
		return nil, err
	}
	if spkg.Version != version {
		return nil, nil
	}
	var pkg lintPackage
	if err := gob.NewDecoder(bytes.NewReader(spkg.Data)).Decode(&pkg); err != nil {
		return nil, err
	}
	return &pkg, nil
}

func runLint(c appengine.Context, importPath string) (*lintPackage, error) {
	dir, err := gosrc.Get(urlfetch.Client(c), importPath, "")
	if err != nil {
		return nil, err
	}

	pkg := lintPackage{
		Path:    importPath,
		Updated: time.Now(),
		LineFmt: dir.LineFmt,
		URL:     dir.BrowseURL,
	}
	linter := lint.Linter{}
	for _, f := range dir.Files {
		if !strings.HasSuffix(f.Name, ".go") {
			continue
		}
		problems, err := linter.Lint(f.Name, f.Data)
		if err == nil && len(problems) == 0 {
			continue
		}
		file := lintFile{Name: f.Name, URL: f.BrowseURL}
		if err != nil {
			file.Problems = []*lintProblem{{Text: err.Error()}}
		} else {
			for _, p := range problems {
				file.Problems = append(file.Problems, &lintProblem{
					Line:       p.Position.Line,
					Text:       p.Text,
					LineText:   p.LineText,
					Confidence: p.Confidence,
				})
			}
		}
		if len(file.Problems) > 0 {
			pkg.Files = append(pkg.Files, &file)
		}
	}

	if err := putPackage(c, importPath, &pkg); err != nil {
		return nil, err
	}

	return &pkg, nil
}

func filterByConfidence(r *http.Request, pkg *lintPackage) {
	minConfidence, err := strconv.ParseFloat(r.FormValue("minConfidence"), 64)
	if err != nil {
		minConfidence = 0.8
	}
	for _, f := range pkg.Files {
		j := 0
		for i := range f.Problems {
			if f.Problems[i].Confidence >= minConfidence {
				f.Problems[j] = f.Problems[i]
				j += 1
			}
		}
		f.Problems = f.Problems[:j]
	}
}

var setupOnce sync.Once

func setup(r *http.Request) {
	c := appengine.NewContext(r)
	c.Infof("Contact email: %s", contactEmail)
	gosrc.SetUserAgent(fmt.Sprintf("%s (+http://%s/-/bot)", appengine.AppID(c), r.Host))
}

type handlerFunc func(http.ResponseWriter, *http.Request) error

func (f handlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setupOnce.Do(func() { setup(r) })
	c := appengine.NewContext(r)
	err := f(w, r)
	if err == nil {
		return
	} else if gosrc.IsNotFound(err) {
		writeErrorResponse(w, 404)
	} else if e, ok := err.(*gosrc.RemoteError); ok {
		c.Infof("Remote error %s: %v", e.Host, e)
		writeResponse(w, 500, errorTemplate, fmt.Sprintf("Error accessing %s.", e.Host))
	} else if err != nil {
		c.Errorf("Internal error %v", err)
		writeErrorResponse(w, 500)
	}
}

func serveRoot(w http.ResponseWriter, r *http.Request) error {
	switch {
	case r.Method != "GET" && r.Method != "HEAD":
		return writeErrorResponse(w, 405)
	case r.URL.Path == "/":
		return writeResponse(w, 200, homeTemplate, nil)
	default:
		importPath := r.URL.Path[1:]
		if !gosrc.IsValidPath(importPath) {
			return gosrc.NotFoundError{Message: "bad path"}
		}
		c := appengine.NewContext(r)
		pkg, err := getPackage(c, importPath)
		if pkg == nil && err == nil {
			pkg, err = runLint(c, importPath)
		}
		if err != nil {
			return err
		}
		filterByConfidence(r, pkg)
		return writeResponse(w, 200, packageTemplate, pkg)
	}
}

func serveRefresh(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "POST" {
		return writeErrorResponse(w, 405)
	}
	importPath := r.FormValue("importPath")
	pkg, err := runLint(appengine.NewContext(r), importPath)
	if err != nil {
		return err
	}
	http.Redirect(w, r, "/"+pkg.Path, 301)
	return nil
}

func serveBot(w http.ResponseWriter, r *http.Request) error {
	c := appengine.NewContext(r)
	_, err := fmt.Fprintf(w, "Contact %s for help with the %s bot.", contactEmail, appengine.AppID(c))
	return err
}
