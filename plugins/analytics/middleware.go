package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/appbaseio-confidential/arc/internal/iplookup"
	"github.com/appbaseio-confidential/arc/internal/types/acl"
	"github.com/appbaseio-confidential/arc/internal/types/index"
	"github.com/appbaseio-confidential/arc/internal/types/op"
	"github.com/appbaseio-confidential/arc/internal/types/user"
	"github.com/appbaseio-confidential/arc/internal/util"
	"github.com/google/uuid"
)

const (
	XSearchQuery         = "X-Search-Query"
	XSearchId            = "X-Search-Id"
	XSearchFilters       = "X-Search-Filters"
	XSearchClick         = "X-Search-Click"
	XSearchClickPosition = "X-Search-Click-Position"
	XSearchConversion    = "X-Search-Conversion"
	XSearchCustomEvent   = "X-Search-Custom-Event"
)

type searchResponse struct {
	Took float64 `json:"took"`
	Hits struct {
		Total int `json:"total"`
		Hits  []struct {
			Source map[string]interface{} `json:"source"`
			Type   string                 `json:"type"`
			Id     string                 `json:"id"`
		} `json:"hits"`
	} `json:"hits"`
}

type mSearchResponse struct {
	Responses []searchResponse `json:"responses"`
}

func (a *analytics) Recorder(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctxACL := ctx.Value(acl.CtxKey)
		if ctxACL == nil {
			log.Printf("%s: unable to fetch acl from request context", logTag)
			util.WriteBackMessage(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		reqACL, ok := ctxACL.(*acl.ACL)
		if !ok {
			log.Printf("%s: unable to cast context acl %v to *acl.ACL", logTag, reqACL)
			util.WriteBackMessage(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		searchQuery := r.Header.Get(XSearchQuery)
		searchId := r.Header.Get(XSearchId)
		if *reqACL != acl.Search || (searchQuery == "" && searchId == "") {
			h(w, r)
			return
		}

		docId := searchId
		if searchId == "" {
			docId = uuid.New().String()
		}

		// serve using response recorder
		respRecorder := httptest.NewRecorder()
		h(respRecorder, r)

		// copy the response to writer
		for k, v := range respRecorder.Header() {
			w.Header()[k] = v
		}
		w.Header().Set(XSearchId, docId)
		w.WriteHeader(respRecorder.Code)
		w.Write(respRecorder.Body.Bytes())

		go a.recordResponse(docId, searchId, respRecorder, r)
	}
}

// TODO: For urls ending with _search or _msearch only?
func (a *analytics) recordResponse(docId, searchId string, respRecorder *httptest.ResponseRecorder, r *http.Request) {
	// read the response from elasticsearch
	respBody, err := ioutil.ReadAll(respRecorder.Result().Body)
	if err != nil {
		log.Printf("%s: can't read response body: %v", logTag, err)
		return
	}

	respBody = bytes.Replace(respBody, []byte("_source"), []byte("source"), -1)
	respBody = bytes.Replace(respBody, []byte("_type"), []byte("type"), -1)
	respBody = bytes.Replace(respBody, []byte("_id"), []byte("id"), -1)

	var esResponse searchResponse
	if strings.Contains(r.RequestURI, "_msearch") {
		var m mSearchResponse
		err := json.Unmarshal(respBody, &m)
		if err != nil {
			log.Printf("%s: unable to unmarshal es reponse %s: %v", logTag, string(respBody), err)
			return
		}
		if len(m.Responses) > 0 {
			esResponse = m.Responses[0]
		}
	} else {
		err := json.Unmarshal(respBody, &esResponse)
		if err != nil {
			log.Printf("%s: unable to unmarshal es reponse %s: %v", logTag, string(respBody), err)
			return
		}
	}

	// record top 10 responses
	var hits []map[string]string
	for i := 0; i < 10 && i < len(esResponse.Hits.Hits); i++ {
		source := esResponse.Hits.Hits[i].Source
		raw, err := json.Marshal(source)
		if err != nil {
			log.Printf("%s: unable to marshal es response source %s: %v", logTag, source, err)
			continue
		}

		h := make(map[string]string)
		h["id"] = esResponse.Hits.Hits[i].Id
		h["type"] = esResponse.Hits.Hits[i].Type
		h["source"] = string(raw)
		hits = append(hits, h)
	}

	record := make(map[string]interface{})
	record["took"] = esResponse.Took
	if searchId == "" {
		record["indices"] = r.Context().Value(index.CtxKey).([]string) // TODO: error check?
		record["search_query"] = r.Header.Get(XSearchQuery)
		record["hits_in_response"] = hits
		record["total_hits"] = esResponse.Hits.Total
		record["datestamp"] = time.Now().Format("2006/01/02 15:04:05")

		searchFilters := parse(r.Header.Get(XSearchFilters))
		if len(searchFilters) > 0 {
			record["search_filters"] = searchFilters
		}
	}

	ipAddr := iplookup.FromRequest(r)
	record["ip"] = ipAddr

	ipInfo := iplookup.New()
	coordinates, err := ipInfo.GetCoordinates(ipAddr)
	if err != nil {
		log.Printf("%s: error fetching location coordinates for ip=%s: %v", logTag, ipAddr, err)
	} else {
		record["location"] = coordinates
	}
	country, err := ipInfo.Get(iplookup.Country, ipAddr)
	if err != nil {
		log.Printf("%s: error fetching country for ip=%s: %v", logTag, ipAddr, err)
	} else {
		record["country"] = country
	}

	searchClick := r.Header.Get(XSearchClick)
	if searchClick != "" {
		if clicked, err := strconv.ParseBool(searchClick); err == nil {
			record["click"] = clicked
		} else {
			log.Printf("%s: invalid bool value '%v' passed for header %s: %v",
				logTag, searchClick, XSearchClick, err)
		}
	}

	searchClickPosition := r.Header.Get(XSearchClickPosition)
	if searchClickPosition != "" {
		if pos, err := strconv.Atoi(searchClickPosition); err == nil {
			record["click_position"] = pos
		} else {
			log.Printf("%s: invalid int value '%v' passed for header %s: %v",
				logTag, searchClickPosition, XSearchClickPosition, err)
		}
	}

	searchConversion := r.Header.Get(XSearchConversion)
	if searchConversion != "" {
		if conversion, err := strconv.ParseBool(searchConversion); err == nil {
			record["conversion"] = conversion
		} else {
			log.Printf("%s: invalid bool value '%v' passed for header %s: %v",
				logTag, searchConversion, XSearchConversion, err)
		}
	}

	customEvents := parse(r.Header.Get(XSearchCustomEvent))
	if len(customEvents) > 0 {
		record["custom_events"] = customEvents
	}

	// TODO: Remove
	//rawRecord, err := json.Marshal(record)
	//if err != nil {
	//	log.Printf("%s: error marshalling analytics record: %v", logTag, err)
	//}
	//log.Printf("%s: %s", logTag, string(rawRecord))

	a.es.indexRecord(docId, record)
}

// classifier classifies an incoming request based on the request method
// and the endpoint it is trying to access. The middleware makes two
// classifications: first, the operation (op.Operation) incoming request is
// trying to do, and second, the acl (acl.ACL) it is trying to access, which
// is acl.User in this case. The identified classifications are passed along
// in the request context to the next middleware. Classifier is supposedly
// the first middleware that a request is expected to encounter.
func classifier(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		analyticsACL := acl.Analytics

		var operation op.Operation
		switch r.Method {
		case http.MethodGet:
			operation = op.Read
		case http.MethodPost:
			operation = op.Write
		case http.MethodPut:
			operation = op.Write
		case http.MethodPatch:
			operation = op.Write
		case http.MethodDelete:
			operation = op.Delete
		default:
			operation = op.Read
		}

		ctx := r.Context()
		ctx = context.WithValue(ctx, acl.CtxKey, &analyticsACL)
		ctx = context.WithValue(ctx, op.CtxKey, &operation)
		r = r.WithContext(ctx)

		h(w, r)
	}
}

// validateOp verifies whether the user.User has the required op.Operation
// in order to access a particular endpoint. The middleware expects the
// request context to have both *user.User who is making the request
// and *op.Operation required in order to access the endpoint. The absence
// of either values in request context will cause the middleware to return
// http.InternalServerError.
func validateOp(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		ctxUser := ctx.Value(user.CtxKey)
		if ctxUser == nil {
			log.Printf("%s: cannot fetch user object from request context", logTag)
			util.WriteBackMessage(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		u := ctxUser.(*user.User)

		ctxOp := ctx.Value(op.CtxKey)
		if ctxOp == nil {
			log.Printf("%s: cannot fetch op from request context", logTag)
			util.WriteBackMessage(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		operation := ctxOp.(*op.Operation)

		if !op.Contains(u.Ops, *operation) {
			msg := fmt.Sprintf("user with user_id=%s does not have '%s' op access",
				u.UserId, operation.String())
			util.WriteBackMessage(w, msg, http.StatusUnauthorized)
			return
		}

		h(w, r)
	}
}

// validateACL verifies whether the user.User has the required acl.ACL in
// order to access a particular endpoint. The middleware expects the request
// context to have *user.User who is making the request. The absence of
// *user.User value in the request context will cause the middleware to return
// http.InternalServerError.
func validateACL(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		ctxUser := ctx.Value(user.CtxKey)
		if ctxUser == nil {
			log.Printf("%s: cannot fetch user object from request context", logTag)
			util.WriteBackMessage(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		u := ctxUser.(*user.User)

		if !acl.Contains(u.ACLs, acl.Analytics) {
			msg := fmt.Sprintf(`user with "user_id"="%s" does not have '%s' acl`,
				u.UserId, acl.Analytics.String())
			util.WriteBackMessage(w, msg, http.StatusUnauthorized)
			return
		}

		h(w, r)
	}
}
