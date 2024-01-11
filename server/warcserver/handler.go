package warcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nlnwa/gowarc"
	"github.com/nlnwa/gowarcserver/index"
	"github.com/nlnwa/gowarcserver/loader"
	"github.com/nlnwa/gowarcserver/server/api"
	"github.com/nlnwa/gowarcserver/server/handlers"
	"github.com/nlnwa/gowarcserver/timestamp"
	urlErrors "github.com/nlnwa/whatwg-url/errors"
	"github.com/nlnwa/whatwg-url/url"
	"github.com/rs/zerolog/log"
)

type Handler struct {
	CdxAPI     index.CdxAPI
	FileAPI    index.FileAPI
	IdAPI      index.IdAPI
	WarcLoader loader.WarcLoader
}

func (h Handler) index(w http.ResponseWriter, r *http.Request) {
	coreAPI, err := api.Parse(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	start := time.Now()
	count := 0
	defer func() {
		log.Debug().Str("request", fmt.Sprintf("%+v", coreAPI)).Msgf("Found %d items in %s", count, time.Since(start))
	}()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	response := make(chan index.CdxResponse)

	if err = h.CdxAPI.Search(ctx, searchApi(coreAPI), response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Error().Err(err).Msgf("Search failed: %+v", coreAPI)
		return
	}

	for res := range response {
		err := res.GetError()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			log.Warn().Err(err).Msg("failed result")
			continue
		}
		cdx := res.GetCdx()
		cdxj, err := json.Marshal(cdxToPywbJson(cdx))
		if err != nil {
			log.Warn().Err(err).Msg("failed to marshal result")
			continue
		}
		switch coreAPI.Output {
		case api.OutputJson:
			_, err = fmt.Fprintln(w, cdxj)
		default:
			ssu := cdx.GetSsu()
			sts := timestamp.TimeTo14(cdx.GetSts().AsTime())
			_, err = fmt.Fprintf(w, "%s %s %s\n", ssu, sts, cdxj)
		}
		if err != nil {
			log.Warn().Err(err).Msg("failed to write result")
			return
		}
		count++
	}
}

// resolveRevisit resolves a revisit record by looking up the closest matching target URI and date
func (h Handler) resolveRevisit(ctx context.Context, targetURI string, closest string) (string, error) {
	uri, err := url.Parse(targetURI)
	if err != nil {
		return "", fmt.Errorf("failed to resolve revisit record: failed to parse WARC-Refers-To-Target-URI: %s: %w", targetURI, err)
	}

	response := make(chan index.CdxResponse)
	err = h.CdxAPI.Search(ctx, api.ClosestAPI(closest, uri), response)
	if err != nil {
		return "", fmt.Errorf("failed to resolve revisit record: failed to get closest match: %s %s: %w", closest, uri, err)
	}

	// we are looking for the record's storage ref
	var ref string

	for res := range response {
		if res.GetError() != nil {
			log.Warn().Err(err).Msgf("error when iterating response of closest search: %s %s", targetURI, closest)
			continue
		}
		ref = res.GetCdx().GetRef()
		break
	}

	return ref, nil
}

func (h Handler) resource(w http.ResponseWriter, r *http.Request) {
	// parse API
	closest, uri := parseResourceRequest(r)
	u, err := url.Parse(uri)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to parse uri '%s': %v", uri, err), http.StatusBadRequest)
		return
	}
	searchAPI := api.ClosestAPI(closest, u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	start := time.Now()
	defer func() {
		log.Debug().Str("request", fmt.Sprintf("%+v", searchAPI.CoreAPI)).Msgf("Fetched resource in %s", time.Since(start))
	}()

	ctx, cancelQuery := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancelQuery()

	// query API
	response := make(chan index.CdxResponse)
	err = h.CdxAPI.Search(ctx, searchAPI, response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Error().Err(err).Msgf("Failed search closest: %+v", searchAPI.CoreAPI)
		return
	}

	var res index.CdxResponse
	for res = range response {
		err := res.GetError()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			log.Warn().Err(err).Msg("Failed cdx response")
			continue
		}
		if res.GetCdx() == nil {
			http.NotFound(w, r)
			return
		}
		break
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ref := res.GetCdx().GetRef()

	// load warc record by storage ref
	var warcRecord gowarc.WarcRecord
	retry := true
	for warcRecord == nil {
		warcRecord, err = h.WarcLoader.LoadByStorageRef(ctx, ref)
		var errResolveRevisit loader.ErrResolveRevisit
		if errors.As(err, &errResolveRevisit) && retry {
			var date string
			if date, err = timestamp.To14(errResolveRevisit.Date); err == nil {
				ref, err = h.resolveRevisit(ctx, errResolveRevisit.TargetURI, date)
			}
			retry = false
		}
		var errWarcRefersToNotFound loader.ErrWarcRefersToNotFound
		if errors.As(err, &errWarcRefersToNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			log.Error().Err(err).Msgf("Failed to load record")
			return
		}
		if err != nil {
			if warcRecord != nil {
				_ = warcRecord.Close()
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Error().Err(err).Msgf("Failed to load record")
			return
		}
	}
	defer warcRecord.Close()

	block, ok := warcRecord.Block().(gowarc.HttpResponseBlock)
	if !ok {
		err := fmt.Errorf("record not renderable: %s", warcRecord)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Error().Err(err).Msg("Failed to load resource")
		return
	}

	s := block.HttpStatusCode()

	if !isRedirect(s) {
		p, err := block.PayloadBytes()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Error().Err(err).Msg("Failed to load resource")
			return
		}
		err = handlers.Render(w, *block.HttpHeader(), block.HttpStatusCode(), p)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to load resource")
		}
		return
	}
	// handle redirect
	location := block.HttpHeader().Get("Location")
	if location == "" {
		err := errors.New("empty redirect location")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Error().Err(err).Msg("Failed to load resource")
		return
	}

	locUrl, err := url.Parse(location)
	if urlErrors.Type(err) == urlErrors.MissingSchemeNonRelativeURL {
		locUrl, err = url.ParseRef(searchAPI.Url.String(), location)
		if err != nil {
			err = fmt.Errorf("failed to parse relative location header as URL: %s: %s: %w", warcRecord, location, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Error().Err(err).Msg("Failed to load resource")
			return
		}
	}
	if err != nil {
		err = fmt.Errorf("failed to parse location header as URL: %s: %s: %w", warcRecord, location, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Error().Err(err).Msg("Failed to load resource")
		return
	}

	response = make(chan index.CdxResponse)
	err = h.CdxAPI.Search(ctx, api.ClosestAPI(closest, locUrl), response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Error().Err(err).Msg("Failed to find closest redirect")
		return
	}

	// fields needed to rewrite the location header
	var sts string
	var loc string

	for res := range response {
		if res.GetError() != nil {
			log.Warn().Err(err).Msg("failed result")
			continue
		}
		cdx := res.GetCdx()
		sts = timestamp.TimeTo14(cdx.GetSts().AsTime())
		loc = cdx.GetUri()
	}
	if loc == "" {
		http.NotFound(w, r)
		return
	}
	before, after, ok := strings.Cut(loc, "?")
	path := r.URL.Path[:strings.Index(r.URL.Path, "id_")-14] + sts + "id_/" + before
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	u, err = url.Parse(scheme + "://" + host)
	if err != nil {
		err := fmt.Errorf("failed to construct redirect location: %s: %w", loc, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Error().Err(err).Msg("Failed to load resource")
		return
	}
	u.SetPathname(path)
	if ok {
		u.SetSearch(after)
	}
	handlers.RenderRedirect(w, u.String())
}

func isRedirect(code int) bool {
	return code == http.StatusMovedPermanently ||
		code == http.StatusFound ||
		code == http.StatusTemporaryRedirect ||
		code == http.StatusPermanentRedirect
}
