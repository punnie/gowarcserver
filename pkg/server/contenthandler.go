/*
 * Copyright 2020 National Library of Norway.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package server

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/nlnwa/gowarc/warcrecord"
	"github.com/nlnwa/gowarcserver/pkg/loader"
	"github.com/nlnwa/gowarcserver/pkg/server/localhttp"
	log "github.com/sirupsen/logrus"
)

type contentHandler struct {
	loader   *loader.Loader
	children *localhttp.Children
}

func (h *contentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	localhttp.FirstQuery(h, w, r, time.Second*3)
}

func (h *contentHandler) ServeLocalHTTP(r *http.Request) (*localhttp.Writer, error) {
	warcid := mux.Vars(r)["id"]
	if len(warcid) > 0 && warcid[0] != '<' {
		warcid = "<" + warcid + ">"
	}

	log.Debugf("request id: %v", warcid)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	record, err := h.loader.Get(ctx, warcid)
	if err != nil {
		return nil, err
	}
	defer record.Close()

	localWriter := localhttp.NewWriter()
	switch v := record.Block().(type) {
	case *warcrecord.RevisitBlock:
		r, err := v.Response()
		if err != nil {
			return nil, err
		}
		renderContent(localWriter, v, r)
	case warcrecord.HttpResponseBlock:
		r, err := v.Response()
		if err != nil {
			return nil, err
		}
		renderContent(localWriter, v, r)
	default:
		localWriter.Header().Set("Content-Type", "text/plain")
		_, err = record.WarcHeader().Write(localWriter)
		if err != nil {
			return nil, err
		}

		rb, err := v.RawBytes()
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(localWriter, rb)
		if err != nil {
			return nil, err
		}
	}
	return localWriter, nil
}

func renderContent(w http.ResponseWriter, v warcrecord.PayloadBlock, r *http.Response) {
	for k, vl := range r.Header {
		for _, v := range vl {
			w.Header().Set(k, v)
		}
	}
	p, err := v.PayloadBytes()
	if err != nil {
		return
	}
	_, err = io.Copy(w, p)
	if err != nil {
		log.Warnf("Failed to writer content for request to %s", r.Request.URL)
	}
}

func (h *contentHandler) PredicateFn(r *http.Response) bool {
	return r.StatusCode == 200
}

func (h *contentHandler) Children() *localhttp.Children {
	return h.children
}
