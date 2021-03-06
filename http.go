// This file is part of *kellner*
//
// Copyright (C) 2015, Travelping GmbH <copyright@travelping.com>
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"
)

type DirEntry struct {
	Name     string
	ModTime  time.Time
	Size     int64
	RawDescr string
	Descr    string
}

type RenderCtx struct {
	Title       string
	Entries     []DirEntry
	SumFileSize int64
	Date        time.Time
	Version     string
}

const TEMPLATE = `<!doctype html>
<title>{{.Title}}</title>
<style type="text/css">
body { font-family: monospace }
td, th { padding: auto 2em }
.col-size { text-align: right }
.col-modtime { white-space: nowrap }
.col-descr { white-space: nowrap }
footer { margin-top: 1em; padding-top: 1em; border-top: 1px dotted silver }
</style>

<p>
This repository contains {{.Entries|len}} packages with an accumulated size of {{.SumFileSize}} bytes.
</p>
<table>
	<thead>
		<tr>
			<th>Name</th>
			<th>Last Modified</th>
			<th>Size</th>
			<th>Description</th>
		</tr>
	</thead>
	<tbody>
{{range .Entries}}
	<tr>
		<td class="col-link"><a href="{{.Name}}">{{.Name}}</a></td>
		<td class="col-modtime">{{.ModTime.Format "2006-01-02T15:04:05Z07:00" }}</td>
		<td class="col-size">{{.Size}}</td>
		<td class="col-descr"><a href="{{.Name}}.control" title="{{.RawDescr | html }}">{{.Descr}}</td>
	</tr>
{{end}}
	</tbody>
</table>

<footer>{{.Version}} - generated at {{.Date}}</footer>
`

var IndexTemplate *template.Template

func init() {
	tmpl, err := template.New("index").Parse(TEMPLATE)
	if err != nil {
		panic(err)
	}
	IndexTemplate = tmpl
}

func AttachHttpHandler(mux *http.ServeMux, packages *PackageIndex, prefix, root string, gzipper Gzipper) {

	now := time.Now()

	packages_stamps := bytes.NewBuffer(nil)
	packages_content := bytes.NewBuffer(nil)
	packages_content_gz := bytes.NewBuffer(nil)
	packages.StringTo(packages_content)
	gzipper(packages_content_gz, bytes.NewReader(packages_content.Bytes()))
	packages.StampsTo(packages_stamps)

	packages_handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			http.ServeContent(w, r, "Packages", now, bytes.NewReader(packages_content.Bytes()))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		http.ServeContent(w, r, "Packages", now, bytes.NewReader(packages_content_gz.Bytes()))
	})

	packages_gz_handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "Packages.gz", now, bytes.NewReader(packages_content_gz.Bytes()))
	})

	packages_stamps_handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "Packages.stamps", now, bytes.NewReader(packages_stamps.Bytes()))
	})

	index_handler := func() http.Handler {

		names := packages.SortedNames()
		ctx := RenderCtx{Title: prefix + " - kellner", Version: VERSION, Date: time.Now()}

		const n_meta_files = 3
		ctx.Entries = make([]DirEntry, len(names)+n_meta_files)
		ctx.Entries[0] = DirEntry{Name: "Packages", ModTime: now, Size: int64(packages_content.Len())}
		ctx.Entries[1] = DirEntry{Name: "Packages.gz", ModTime: now, Size: int64(packages_content_gz.Len())}
		ctx.Entries[2] = DirEntry{Name: "Packages.stamps", ModTime: now, Size: int64(packages_stamps.Len())}

		for i, name := range names {
			ipkg := packages.Entries[name]
			ctx.Entries[i+n_meta_files] = ipkg.DirEntry()
			ctx.SumFileSize += ipkg.FileInfo.Size()
		}

		index, index_gz := ctx.render(IndexTemplate)

		// the actual index handler
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".control") {
				ipkg_name := r.URL.Path[:len(r.URL.Path)-8]
				ipkg, ok := packages.Entries[path.Base(ipkg_name)]
				if !ok {
					http.NotFound(w, r)
					return
				}
				io.WriteString(w, ipkg.Control)
			} else if r.URL.Path == prefix || r.URL.Path == prefix+"/" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
					w.Write(index.Bytes())
					return
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Write(index_gz.Bytes())
			} else {
				http.ServeFile(w, r, path.Join(root, r.URL.Path))
			}
		})
	}()

	mux.Handle(prefix+"/", index_handler)
	mux.Handle(prefix+"/Packages", packages_handler)
	mux.Handle(prefix+"/Packages.gz", packages_gz_handler)
	mux.Handle(prefix+"/Packages.stamps", packages_stamps_handler)
}

func (ctx *RenderCtx) render(tmpl *template.Template) (index, index_gz *bytes.Buffer) {

	index = bytes.NewBuffer(nil)
	if err := IndexTemplate.Execute(index, ctx); err != nil {
		panic(err)
	}
	index_gz = bytes.NewBuffer(nil)
	gz := gzip.NewWriter(index_gz)
	gz.Write(index.Bytes())
	gz.Close()

	return index, index_gz
}

// based upon 'feeds' create a opkg-repository snippet:
//
//   src/gz name-ipks http://host:port/name
//   src/gz name2-ipks http://host:port/name2
//
// TODO: add that entry to the parent directory-handler "somehow"
func AttachOpkgRepoSnippet(mux *http.ServeMux, mount string, feeds []string) {

	mux.Handle(mount, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		scheme := r.URL.Scheme
		if scheme == "" {
			scheme = "http://"
		}

		for _, mux_path := range feeds {
			repo_name := strings.Replace(mux_path[1:], "/", "-", -1)
			fmt.Fprintf(w, "src/gz %s-ipks %s%s%s\n", repo_name, scheme, r.Host, mux_path)
		}
	}))
}

const _EXTRA_LOG_KEY = "kellner-log-data"

// wraps 'orig_handler' to log incoming http-request
func logRequests(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// NOTE: maybe a dopey idea: let the http-handlers attach logging
		// data to the request. pro: it hijacks a data structure meant to transport
		// external data
		//
		// only internal handlers are allowed to attach data to the
		// request to hand log-data over to this handler here. to make
		// sure external sources do not have control over our logs: delete
		// any existing data before starting the handler-chain.
		r.Header.Del(_EXTRA_LOG_KEY)

		status_log := logStatusCode{ResponseWriter: w}
		handler.ServeHTTP(&status_log, r)
		if status_log.Code == 0 {
			status_log.Code = 200
		}

		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			log.Println(r.RemoteAddr, r.Method, status_log.Code, r.Host, r.RequestURI, r.Header)
			return
		}

		// TODO: handle more than the first certificate
		clientId := clientIdByName(&r.TLS.PeerCertificates[0].Subject)
		log.Println(r.RemoteAddr, clientId, r.Method, status_log.Code, r.Host, r.RequestURI, r.Header)
	})
}

//
// small helper to intercept the http-statuscode written
// to the original http.ResponseWriter
type logStatusCode struct {
	http.ResponseWriter
	Code int
}

func (w *logStatusCode) WriteHeader(code int) {
	w.Code = code
	w.ResponseWriter.WriteHeader(code)
}
