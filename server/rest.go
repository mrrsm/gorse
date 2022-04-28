// Copyright 2020 gorse Project Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"github.com/araddon/dateparse"
	restfulspec "github.com/emicklei/go-restful-openapi/v2"
	"github.com/emicklei/go-restful/v3"
	"github.com/juju/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samber/lo"
	"github.com/scylladb/go-set"
	"github.com/scylladb/go-set/strset"
	"github.com/thoas/go-funk"
	"github.com/zhenghaoz/gorse/base"
	"github.com/zhenghaoz/gorse/base/heap"
	"github.com/zhenghaoz/gorse/config"
	"github.com/zhenghaoz/gorse/storage/cache"
	"github.com/zhenghaoz/gorse/storage/data"
	"go.uber.org/zap"
	"math"
	"modernc.org/mathutil"
	"net/http"
	"strconv"
	"time"
)

// RestServer implements a REST-ful API server.
type RestServer struct {
	CacheClient cache.Database
	DataClient  data.Database
	GorseConfig *config.Config
	HttpHost    string
	HttpPort    int
	IsDashboard bool
	DisableLog  bool
	WebService  *restful.WebService

	PopularItemsCache *PopularItemsCache
	HiddenItemsCache  *HiddenItemsCache
}

// StartHttpServer starts the REST-ful API server.
func (s *RestServer) StartHttpServer() {
	// register restful APIs
	s.CreateWebService()
	restful.DefaultContainer.Add(s.WebService)
	// register swagger UI
	specConfig := restfulspec.Config{
		WebServices: restful.RegisteredWebServices(),
		APIPath:     "/apidocs.json",
	}
	restful.DefaultContainer.Add(restfulspec.NewOpenAPIService(specConfig))
	swaggerFile = specConfig.APIPath
	http.HandleFunc(apiDocsPath, handler)
	// register prometheus
	http.Handle("/metrics", promhttp.Handler())

	base.Logger().Info("start http server",
		zap.String("url", fmt.Sprintf("http://%s:%d", s.HttpHost, s.HttpPort)))
	base.Logger().Fatal("failed to start http server",
		zap.Error(http.ListenAndServe(fmt.Sprintf("%s:%d", s.HttpHost, s.HttpPort), nil)))
}

func (s *RestServer) LogFilter(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	chain.ProcessFilter(req, resp)
	if !s.DisableLog && req.Request.URL.Path != "/api/dashboard/cluster" &&
		req.Request.URL.Path != "/api/dashboard/tasks" {
		base.Logger().Info(fmt.Sprintf("%s %s", req.Request.Method, req.Request.URL),
			zap.Int("status_code", resp.StatusCode()))
	}
}

func (s *RestServer) AuthFilter(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	if s.IsDashboard || s.GorseConfig.Server.APIKey == "" {
		chain.ProcessFilter(req, resp)
		return
	}
	apikey := req.HeaderParameter("X-API-Key")
	if apikey == s.GorseConfig.Server.APIKey {
		chain.ProcessFilter(req, resp)
		return
	}
	base.Logger().Error("unauthorized",
		zap.String("api_key", s.GorseConfig.Server.APIKey),
		zap.String("X-API-Key", apikey))
	if err := resp.WriteError(http.StatusUnauthorized, fmt.Errorf("unauthorized")); err != nil {
		base.Logger().Error("failed to write error", zap.Error(err))
	}
}

// CreateWebService creates web service.
func (s *RestServer) CreateWebService() {
	// Create a server
	ws := s.WebService
	ws.Path("/api/").
		Produces(restful.MIME_JSON).
		Filter(s.LogFilter).
		Filter(s.AuthFilter)

	/* Interactions with data store */

	// Insert a user
	ws.Route(ws.POST("/user").To(s.insertUser).
		Doc("Insert a user.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"user"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Returns(200, "OK", Success{}).
		Reads(data.User{}))
	// Modify a user
	ws.Route(ws.PATCH("/user/{user-id}").To(s.modifyUser).
		Doc("Modify a user.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"user"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Reads(data.UserPatch{}).
		Returns(200, "OK", Success{}))
	// Get a user
	ws.Route(ws.GET("/user/{user-id}").To(s.getUser).
		Doc("Get a user.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"user"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Returns(200, "OK", data.User{}).
		Writes(data.User{}))
	// Insert users
	ws.Route(ws.POST("/users").To(s.insertUsers).
		Doc("Insert users.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"user"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Returns(200, "OK", Success{}).
		Reads([]data.User{}))
	// Get users
	ws.Route(ws.GET("/users").To(s.getUsers).
		Doc("Get users.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"user"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned users").DataType("integer")).
		Param(ws.QueryParameter("cursor", "cursor for next page").DataType("string")).
		Returns(200, "OK", UserIterator{}).
		Writes(UserIterator{}))
	// Delete a user
	ws.Route(ws.DELETE("/user/{user-id}").To(s.deleteUser).
		Doc("Delete a user and his or her feedback.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"user"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Returns(200, "OK", Success{}).
		Writes(Success{}))

	// Insert an item
	ws.Route(ws.POST("/item").To(s.insertItem).
		Doc("Insert an item. Overwrite if the item exists.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Returns(200, "OK", Success{}).
		Reads(data.Item{}))
	// Modify an item
	ws.Route(ws.PATCH("/item/{item-id}").To(s.modifyItem).
		Doc("Modify an item.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Reads(data.ItemPatch{}).
		Returns(200, "OK", Success{}))
	// Get items
	ws.Route(ws.GET("/items").To(s.getItems).
		Doc("Get items.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("cursor", "cursor for next page").DataType("string")).
		Returns(200, "OK", ItemIterator{}).
		Writes(ItemIterator{}))
	// Get item
	ws.Route(ws.GET("/item/{item-id}").To(s.getItem).
		Doc("Get a item.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Returns(200, "OK", data.Item{}).
		Writes(data.Item{}))
	// Insert items
	ws.Route(ws.POST("/items").To(s.insertItems).
		Doc("Insert items. Overwrite if items exist").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Reads([]data.Item{}))
	// Delete item
	ws.Route(ws.DELETE("/item/{item-id}").To(s.deleteItem).
		Doc("Delete an item and its feedback.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Returns(200, "OK", Success{}).
		Writes(Success{}))
	// Insert category
	ws.Route(ws.PUT("/item/{item-id}/category/{category}").To(s.insertItemCategory).
		Doc("Insert a category for a item").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Param(ws.PathParameter("category", "item category").DataType("string")).
		Returns(200, "OK", Success{}).
		Writes(Success{}))
	// Delete category
	ws.Route(ws.DELETE("/item/{item-id}/category/{category}").To(s.deleteItemCategory).
		Doc("Delete a category from a item").
		Metadata(restfulspec.KeyOpenAPITags, []string{"item"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Param(ws.PathParameter("category", "item category").DataType("string")).
		Returns(200, "OK", Success{}).
		Writes(Success{}))

	// Insert feedback
	ws.Route(ws.POST("/feedback").To(s.insertFeedback(false)).
		Doc("Insert feedbacks. Ignore insertion if feedback exists.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Reads([]data.Feedback{}).
		Returns(200, "OK", Success{}))
	ws.Route(ws.PUT("/feedback").To(s.insertFeedback(true)).
		Doc("Insert feedbacks. Existed feedback will be overwritten.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Reads([]data.Feedback{}).
		Returns(200, "OK", Success{}))
	// Get feedback
	ws.Route(ws.GET("/feedback").To(s.getFeedback).
		Doc("Get feedbacks.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.QueryParameter("cursor", "cursor for next page").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned feedback").DataType("integer")).
		Returns(200, "OK", FeedbackIterator{}).
		Writes(FeedbackIterator{}))
	ws.Route(ws.GET("/feedback/{user-id}/{item-id}").To(s.getUserItemFeedback).
		Doc("Get feedbacks between a user and a item.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Returns(200, "OK", []data.Feedback{}).
		Writes([]data.Feedback{}))
	ws.Route(ws.DELETE("/feedback/{user-id}/{item-id}").To(s.deleteUserItemFeedback).
		Doc("Delete feedbacks between a user and a item.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Returns(200, "OK", []data.Feedback{}).
		Writes([]data.Feedback{}))
	ws.Route(ws.GET("/feedback/{feedback-type}").To(s.getTypedFeedback).
		Doc("Get feedbacks with feedback type.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("feedback-type", "feedback type").DataType("string")).
		Param(ws.QueryParameter("cursor", "cursor for next page").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned feedback").DataType("integer")).
		Returns(200, "OK", FeedbackIterator{}).
		Writes(FeedbackIterator{}))
	ws.Route(ws.GET("/feedback/{feedback-type}/{user-id}/{item-id}").To(s.getTypedUserItemFeedback).
		Doc("Get feedbacks between a user and a item with feedback type.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("feedback-type", "feedback type").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Returns(200, "OK", data.Feedback{}).
		Writes(data.Feedback{}))
	ws.Route(ws.DELETE("/feedback/{feedback-type}/{user-id}/{item-id}").To(s.deleteTypedUserItemFeedback).
		Doc("Delete feedbacks between a user and a item with feedback type.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("feedback-type", "feedback type").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Returns(200, "OK", data.Feedback{}).
		Writes(data.Feedback{}))
	// Get feedback by user id
	ws.Route(ws.GET("/user/{user-id}/feedback/{feedback-type}").To(s.getTypedFeedbackByUser).
		Doc("Get feedbacks by user id with feedback type.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.PathParameter("feedback-type", "feedback type").DataType("string")).
		Returns(200, "OK", []data.Feedback{}).
		Writes([]data.Feedback{}))
	ws.Route(ws.GET("/user/{user-id}/feedback").To(s.getFeedbackByUser).
		Doc("Get feedbacks by user id.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Returns(200, "OK", []data.Feedback{}).
		Writes([]data.Feedback{}))
	// Get feedback by item-id
	ws.Route(ws.GET("/item/{item-id}/feedback/{feedback-type}").To(s.getTypedFeedbackByItem).
		Doc("Get feedbacks by item id with feedback type.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Param(ws.PathParameter("feedback-type", "feedback type").DataType("string")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.GET("/item/{item-id}/feedback/").To(s.getFeedbackByItem).
		Doc("Get feedbacks by item id.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"feedback"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))

	/* Interaction with intermediate result */

	// Get collaborative filtering recommendation by user id
	ws.Route(ws.GET("/intermediate/recommend/{user-id}").To(s.getCollaborative).
		Doc("get the collaborative filtering recommendation for a user").
		Metadata(restfulspec.KeyOpenAPITags, []string{"intermediate"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of the list").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.GET("/intermediate/recommend/{user-id}/{category}").To(s.getCategorizedCollaborative).
		Doc("get the collaborative filtering recommendation for a user").
		Metadata(restfulspec.KeyOpenAPITags, []string{"intermediate"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "identifier of the user").DataType("string")).
		Param(ws.PathParameter("category", "category of items").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of the list").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))

	/* Rank recommendation */

	// Get popular items
	ws.Route(ws.GET("/popular").To(s.getPopular).
		Doc("Get popular items").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned recommendations").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned recommendations").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.GET("/popular/{category}").To(s.getPopular).
		Doc("Get popular items in category").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("category", "item category").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Returns(http.StatusOK, "OK", []string{}).
		Writes([]string{}))
	// Get latest items
	ws.Route(ws.GET("/latest").To(s.getLatest).
		Doc("get latest items").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Returns(200, "OK", []cache.Scored{}).
		Writes([]cache.Scored{}))
	ws.Route(ws.GET("/latest/{category}").To(s.getLatest).
		Doc("get latest items in category").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("category", "items category").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Returns(http.StatusOK, "OK", []string{}).
		Writes([]string{}))
	// Get neighbors
	ws.Route(ws.GET("/item/{item-id}/neighbors/").To(s.getItemNeighbors).
		Doc("get neighbors of a item").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.GET("/item/{item-id}/neighbors/{category}").To(s.getItemCategorizedNeighbors).
		Doc("get neighbors of a item").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("item-id", "item id").DataType("string")).
		Param(ws.PathParameter("category", "item category").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.GET("/user/{user-id}/neighbors/").To(s.getUserNeighbors).
		Doc("get neighbors of a user").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned users").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned users").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.GET("/recommend/{user-id}").To(s.getRecommend).
		Doc("Get recommendation for user.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.QueryParameter("write-back-type", "type of write back feedback").DataType("string")).
		Param(ws.QueryParameter("write-back-delay", "timestamp delay of write back feedback").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.GET("/recommend/{user-id}/{category}").To(s.getRecommend).
		Doc("Get recommendation for user.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("user-id", "user id").DataType("string")).
		Param(ws.PathParameter("category", "item category").DataType("string")).
		Param(ws.QueryParameter("write-back-type", "type of write back feedback").DataType("string")).
		Param(ws.QueryParameter("write-back-delay", "timestamp delay of write back feedback").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.POST("/session/recommend").To(s.sessionRecommend).
		Doc("Get recommendation for session.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Reads([]Feedback{}).
		Returns(200, "OK", []string{}).
		Writes([]string{}))
	ws.Route(ws.POST("/session/recommend/{category}").To(s.sessionRecommend).
		Doc("Get recommendation for session.").
		Metadata(restfulspec.KeyOpenAPITags, []string{"recommendation"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.PathParameter("category", "item category").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned items").DataType("integer")).
		Param(ws.QueryParameter("offset", "offset of returned items").DataType("integer")).
		Reads([]Feedback{}).
		Returns(200, "OK", []string{}).
		Writes([]string{}))

	/* Interaction with measurements */

	ws.Route(ws.GET("/measurements/{name}").To(s.getMeasurements).
		Doc("Get measurements").
		Metadata(restfulspec.KeyOpenAPITags, []string{"measurements"}).
		Param(ws.HeaderParameter("X-API-Key", "api key").DataType("string")).
		Param(ws.QueryParameter("n", "number of returned measurements.").DataType("integer")).
		Returns(200, "OK", []data.Measurement{}).
		Writes([]data.Measurement{}))
}

// ParseInt parses integers from the query parameter.
func ParseInt(request *restful.Request, name string, fallback int) (value int, err error) {
	valueString := request.QueryParameter(name)
	value, err = strconv.Atoi(valueString)
	if err != nil && valueString == "" {
		value = fallback
		err = nil
	}
	return
}

// ParseDuration parses duration from the query parameter.
func ParseDuration(request *restful.Request, name string) (time.Duration, error) {
	valueString := request.QueryParameter(name)
	if valueString == "" {
		return 0, nil
	}
	return time.ParseDuration(valueString)
}

func (s *RestServer) getSort(key string, request *restful.Request, response *restful.Response) {
	var n, offset int
	var err error
	// read arguments
	if offset, err = ParseInt(request, "offset", 0); err != nil {
		BadRequest(response, err)
		return
	}
	if n, err = ParseInt(request, "n", s.GorseConfig.Server.DefaultN); err != nil {
		BadRequest(response, err)
		return
	}
	// Get the popular list
	items, err := s.CacheClient.GetSorted(key, offset, s.GorseConfig.Recommend.CacheSize)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	items = s.FilterOutHiddenScores(items)
	if n > 0 && len(items) > n {
		items = items[:n]
	}
	// Send result
	Ok(response, items)
}

func (s *RestServer) getPopular(request *restful.Request, response *restful.Response) {
	category := request.PathParameter("category")
	base.Logger().Debug("get category popular items in category", zap.String("category", category))
	s.getSort(cache.Key(cache.PopularItems, category), request, response)
}

func (s *RestServer) getLatest(request *restful.Request, response *restful.Response) {
	category := request.PathParameter("category")
	base.Logger().Debug("get category latest items in category", zap.String("category", category))
	s.getSort(cache.Key(cache.LatestItems, category), request, response)
}

// get feedback by item-id with feedback type
func (s *RestServer) getTypedFeedbackByItem(request *restful.Request, response *restful.Response) {
	feedbackType := request.PathParameter("feedback-type")
	itemId := request.PathParameter("item-id")
	feedback, err := s.DataClient.GetItemFeedback(itemId, feedbackType)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, feedback)
}

// get feedback by item-id
func (s *RestServer) getFeedbackByItem(request *restful.Request, response *restful.Response) {
	itemId := request.PathParameter("item-id")
	feedback, err := s.DataClient.GetItemFeedback(itemId)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, feedback)
}

// getItemNeighbors gets neighbors of a item from database.
func (s *RestServer) getItemNeighbors(request *restful.Request, response *restful.Response) {
	// Get item id
	itemId := request.PathParameter("item-id")
	s.getSort(cache.Key(cache.ItemNeighbors, itemId), request, response)
}

// getItemCategorizedNeighbors gets categorized neighbors of an item from database.
func (s *RestServer) getItemCategorizedNeighbors(request *restful.Request, response *restful.Response) {
	// Get item id
	itemId := request.PathParameter("item-id")
	category := request.PathParameter("category")
	s.getSort(cache.Key(cache.ItemNeighbors, itemId, category), request, response)
}

// getUserNeighbors gets neighbors of a user from database.
func (s *RestServer) getUserNeighbors(request *restful.Request, response *restful.Response) {
	// Get item id
	userId := request.PathParameter("user-id")
	s.getSort(cache.Key(cache.UserNeighbors, userId), request, response)
}

// getSubscribe gets subscribed items of a user from database.
//func (s *RestServer) getSubscribe(request *restful.Request, response *restful.Response) {
//	// Authorize
//	if !s.auth(request, response) {
//		return
//	}
//	// Get user id
//	userId := request.PathParameter("user-id")
//	s.getList(cache.SubscribeItems, userId, request, response)
//}

// getCategorizedCollaborative gets cached categorized recommended items from database.
func (s *RestServer) getCategorizedCollaborative(request *restful.Request, response *restful.Response) {
	// Get user id
	userId := request.PathParameter("user-id")
	category := request.PathParameter("category")
	s.getSort(cache.Key(cache.OfflineRecommend, userId, category), request, response)
}

// getCollaborative gets cached recommended items from database.
func (s *RestServer) getCollaborative(request *restful.Request, response *restful.Response) {
	// Get user id
	userId := request.PathParameter("user-id")
	s.getSort(cache.Key(cache.OfflineRecommend, userId), request, response)
}

// Recommend items to users.
// 1. If there are recommendations in cache, return cached recommendations.
// 2. If there are historical interactions of the users, return similar items.
// 3. Otherwise, return fallback recommendation (popular/latest).
func (s *RestServer) Recommend(userId, category string, n int, recommenders ...Recommender) ([]string, error) {
	initStart := time.Now()

	// create context
	ctx, err := s.createRecommendContext(userId, category, n)
	if err != nil {
		return nil, errors.Trace(err)
	}

	// execute recommenders
	for _, recommender := range recommenders {
		err = recommender(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// return recommendations
	if len(ctx.results) > n {
		ctx.results = ctx.results[:n]
	}
	totalTime := time.Since(initStart)
	base.Logger().Info("complete recommendation",
		zap.Int("num_from_final", ctx.numFromOffline),
		zap.Int("num_from_collaborative", ctx.numFromCollaborative),
		zap.Int("num_from_item_based", ctx.numFromItemBased),
		zap.Int("num_from_user_based", ctx.numFromUserBased),
		zap.Int("num_from_latest", ctx.numFromLatest),
		zap.Int("num_from_poplar", ctx.numFromPopular),
		zap.Duration("total_time", totalTime),
		zap.Duration("load_final_recommend_time", ctx.loadOfflineRecTime),
		zap.Duration("load_col_recommend_time", ctx.loadColRecTime),
		zap.Duration("load_hist_time", ctx.loadLoadHistTime),
		zap.Duration("item_based_recommend_time", ctx.itemBasedTime),
		zap.Duration("user_based_recommend_time", ctx.userBasedTime),
		zap.Duration("load_latest_time", ctx.loadLatestTime),
		zap.Duration("load_popular_time", ctx.loadPopularTime))
	return ctx.results, nil
}

type recommendContext struct {
	userId       string
	category     string
	userFeedback []data.Feedback
	n            int
	results      []string
	excludeSet   *strset.Set

	numPrevStage         int
	numFromLatest        int
	numFromPopular       int
	numFromUserBased     int
	numFromItemBased     int
	numFromCollaborative int
	numFromOffline       int

	loadOfflineRecTime time.Duration
	loadColRecTime     time.Duration
	loadLoadHistTime   time.Duration
	itemBasedTime      time.Duration
	userBasedTime      time.Duration
	loadLatestTime     time.Duration
	loadPopularTime    time.Duration
}

func (s *RestServer) createRecommendContext(userId, category string, n int) (*recommendContext, error) {
	// pull ignored items
	ignoreItems, err := s.CacheClient.GetSortedByScore(cache.Key(cache.IgnoreItems, userId),
		math.Inf(-1), float64(time.Now().Add(s.GorseConfig.Server.ClockError).Unix()))
	if err != nil {
		return nil, errors.Trace(err)
	}
	excludeSet := strset.New()
	for _, item := range ignoreItems {
		excludeSet.Add(item.Id)
	}
	return &recommendContext{
		userId:     userId,
		category:   category,
		n:          n,
		excludeSet: excludeSet,
	}, nil
}

func (s *RestServer) requireUserFeedback(ctx *recommendContext) error {
	if ctx.userFeedback == nil {
		start := time.Now()
		var err error
		ctx.userFeedback, err = s.DataClient.GetUserFeedback(ctx.userId, false)
		if err != nil {
			return errors.Trace(err)
		}
		for _, feedback := range ctx.userFeedback {
			ctx.excludeSet.Add(feedback.ItemId)
		}
		ctx.loadLoadHistTime = time.Since(start)
	}
	return nil
}

func (s *RestServer) FilterOutHiddenScores(items []cache.Scored) []cache.Scored {
	isHidden, err := s.HiddenItemsCache.IsHidden(cache.RemoveScores(items))
	if err != nil {
		base.Logger().Error("failed to check hidden items", zap.Error(err))
		return items
	}
	results := make([]cache.Scored, 0, len(items))
	for i := range isHidden {
		if !isHidden[i] {
			results = append(results, items[i])
		}
	}
	return results
}

func (s *RestServer) filterOutHiddenFeedback(feedbacks []data.Feedback) []data.Feedback {
	names := make([]string, len(feedbacks))
	for i, item := range feedbacks {
		names[i] = item.ItemId
	}
	isHidden, err := s.HiddenItemsCache.IsHidden(names)
	if err != nil {
		base.Logger().Error("failed to check hidden items", zap.Error(err))
		return feedbacks
	}
	var results []data.Feedback
	for i := range isHidden {
		if !isHidden[i] {
			results = append(results, feedbacks[i])
		}
	}
	return results
}

type Recommender func(ctx *recommendContext) error

func (s *RestServer) RecommendOffline(ctx *recommendContext) error {
	if len(ctx.results) < ctx.n {
		start := time.Now()
		recommendation, err := s.CacheClient.GetSorted(cache.Key(cache.OfflineRecommend, ctx.userId, ctx.category), 0, s.GorseConfig.Recommend.CacheSize)
		if err != nil {
			return errors.Trace(err)
		}
		recommendation = s.FilterOutHiddenScores(recommendation)
		for _, item := range recommendation {
			if !ctx.excludeSet.Has(item.Id) {
				ctx.results = append(ctx.results, item.Id)
				ctx.excludeSet.Add(item.Id)
			}
		}
		ctx.loadOfflineRecTime = time.Since(start)
		LoadCTRRecommendCacheSeconds.Observe(ctx.loadOfflineRecTime.Seconds())
		ctx.numFromOffline = len(ctx.results) - ctx.numPrevStage
		ctx.numPrevStage = len(ctx.results)
	}
	return nil
}

func (s *RestServer) RecommendCollaborative(ctx *recommendContext) error {
	if len(ctx.results) < ctx.n {
		start := time.Now()
		collaborativeRecommendation, err := s.CacheClient.GetSorted(cache.Key(cache.CollaborativeRecommend, ctx.userId, ctx.category), 0, s.GorseConfig.Recommend.CacheSize)
		if err != nil {
			return errors.Trace(err)
		}
		collaborativeRecommendation = s.FilterOutHiddenScores(collaborativeRecommendation)
		for _, item := range collaborativeRecommendation {
			if !ctx.excludeSet.Has(item.Id) {
				ctx.results = append(ctx.results, item.Id)
				ctx.excludeSet.Add(item.Id)
			}
		}
		ctx.loadColRecTime = time.Since(start)
		LoadCollaborativeRecommendCacheSeconds.Observe(ctx.loadColRecTime.Seconds())
		ctx.numFromCollaborative = len(ctx.results) - ctx.numPrevStage
		ctx.numPrevStage = len(ctx.results)
	}
	return nil
}

func (s *RestServer) RecommendUserBased(ctx *recommendContext) error {
	if len(ctx.results) < ctx.n {
		err := s.requireUserFeedback(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		start := time.Now()
		candidates := make(map[string]float64)
		// load similar users
		similarUsers, err := s.CacheClient.GetSorted(cache.Key(cache.UserNeighbors, ctx.userId), 0, s.GorseConfig.Recommend.CacheSize)
		if err != nil {
			return errors.Trace(err)
		}
		for _, user := range similarUsers {
			// load historical feedback
			feedbacks, err := s.DataClient.GetUserFeedback(user.Id, false, s.GorseConfig.Recommend.DataSource.PositiveFeedbackTypes...)
			if err != nil {
				return errors.Trace(err)
			}
			feedbacks = s.filterOutHiddenFeedback(feedbacks)
			// add unseen items
			for _, feedback := range feedbacks {
				if !ctx.excludeSet.Has(feedback.ItemId) {
					item, err := s.DataClient.GetItem(feedback.ItemId)
					if err != nil {
						return errors.Trace(err)
					}
					if ctx.category == "" || funk.ContainsString(item.Categories, ctx.category) {
						candidates[feedback.ItemId] += user.Score
					}
				}
			}
		}
		// collect top k
		k := ctx.n - len(ctx.results)
		filter := heap.NewTopKStringFilter(k)
		for id, score := range candidates {
			filter.Push(id, score)
		}
		ids, _ := filter.PopAll()
		ctx.results = append(ctx.results, ids...)
		ctx.excludeSet.Add(ids...)
		ctx.userBasedTime = time.Since(start)
		UserBasedRecommendSeconds.Observe(ctx.userBasedTime.Seconds())
		ctx.numFromUserBased = len(ctx.results) - ctx.numPrevStage
		ctx.numPrevStage = len(ctx.results)
	}
	return nil
}

func (s *RestServer) RecommendItemBased(ctx *recommendContext) error {
	if len(ctx.results) < ctx.n {
		err := s.requireUserFeedback(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		start := time.Now()
		// truncate user feedback
		data.SortFeedbacks(ctx.userFeedback)
		userFeedback := make([]data.Feedback, 0, s.GorseConfig.Recommend.Online.NumFeedbackFallbackItemBased)
		for _, feedback := range ctx.userFeedback {
			if s.GorseConfig.Recommend.Online.NumFeedbackFallbackItemBased <= len(userFeedback) {
				break
			}
			if funk.ContainsString(s.GorseConfig.Recommend.DataSource.PositiveFeedbackTypes, feedback.FeedbackType) {
				userFeedback = append(userFeedback, feedback)
			}
		}
		// collect candidates
		candidates := make(map[string]float64)
		for _, feedback := range userFeedback {
			// load similar items
			similarItems, err := s.CacheClient.GetSorted(cache.Key(cache.ItemNeighbors, feedback.ItemId, ctx.category), 0, s.GorseConfig.Recommend.CacheSize)
			if err != nil {
				return errors.Trace(err)
			}
			// add unseen items
			similarItems = s.FilterOutHiddenScores(similarItems)
			for _, item := range similarItems {
				if !ctx.excludeSet.Has(item.Id) {
					candidates[item.Id] += item.Score
				}
			}
		}
		// collect top k
		k := ctx.n - len(ctx.results)
		filter := heap.NewTopKStringFilter(k)
		for id, score := range candidates {
			filter.Push(id, score)
		}
		ids, _ := filter.PopAll()
		ctx.results = append(ctx.results, ids...)
		ctx.excludeSet.Add(ids...)
		ctx.itemBasedTime = time.Since(start)
		ItemBasedRecommendSeconds.Observe(ctx.itemBasedTime.Seconds())
		ctx.numFromItemBased = len(ctx.results) - ctx.numPrevStage
		ctx.numPrevStage = len(ctx.results)
	}
	return nil
}

func (s *RestServer) RecommendLatest(ctx *recommendContext) error {
	if len(ctx.results) < ctx.n {
		err := s.requireUserFeedback(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		start := time.Now()
		items, err := s.CacheClient.GetSorted(cache.Key(cache.LatestItems, ctx.category), 0, s.GorseConfig.Recommend.CacheSize)
		if err != nil {
			return errors.Trace(err)
		}
		items = s.FilterOutHiddenScores(items)
		for _, item := range items {
			if !ctx.excludeSet.Has(item.Id) {
				ctx.results = append(ctx.results, item.Id)
				ctx.excludeSet.Add(item.Id)
			}
		}
		ctx.loadLatestTime = time.Since(start)
		LoadLatestRecommendCacheSeconds.Observe(ctx.loadLatestTime.Seconds())
		ctx.numFromLatest = len(ctx.results) - ctx.numPrevStage
		ctx.numPrevStage = len(ctx.results)
	}
	return nil
}

func (s *RestServer) RecommendPopular(ctx *recommendContext) error {
	if len(ctx.results) < ctx.n {
		err := s.requireUserFeedback(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		start := time.Now()
		items, err := s.CacheClient.GetSorted(cache.Key(cache.PopularItems, ctx.category), 0, s.GorseConfig.Recommend.CacheSize)
		if err != nil {
			return errors.Trace(err)
		}
		items = s.FilterOutHiddenScores(items)
		for _, item := range items {
			if !ctx.excludeSet.Has(item.Id) {
				ctx.results = append(ctx.results, item.Id)
				ctx.excludeSet.Add(item.Id)
			}
		}
		ctx.loadPopularTime = time.Since(start)
		LoadPopularRecommendCacheSeconds.Observe(ctx.loadPopularTime.Seconds())
		ctx.numFromPopular = len(ctx.results) - ctx.numPrevStage
		ctx.numPrevStage = len(ctx.results)
	}
	return nil
}

func (s *RestServer) getRecommend(request *restful.Request, response *restful.Response) {
	startTime := time.Now()
	// parse arguments
	userId := request.PathParameter("user-id")
	n, err := ParseInt(request, "n", s.GorseConfig.Server.DefaultN)
	if err != nil {
		BadRequest(response, err)
		return
	}
	category := request.PathParameter("category")
	offset, err := ParseInt(request, "offset", 0)
	if err != nil {
		BadRequest(response, err)
		return
	}
	writeBackFeedback := request.QueryParameter("write-back-type")
	writeBackDelay, err := ParseDuration(request, "write-back-delay")
	if err != nil {
		BadRequest(response, err)
		return
	}
	// online recommendation
	recommenders := []Recommender{s.RecommendOffline}
	for _, recommender := range s.GorseConfig.Recommend.Online.FallbackRecommend {
		switch recommender {
		case "collaborative":
			recommenders = append(recommenders, s.RecommendCollaborative)
		case "item_based":
			recommenders = append(recommenders, s.RecommendItemBased)
		case "user_based":
			recommenders = append(recommenders, s.RecommendUserBased)
		case "latest":
			recommenders = append(recommenders, s.RecommendLatest)
		case "popular":
			recommenders = append(recommenders, s.RecommendPopular)
		default:
			InternalServerError(response, fmt.Errorf("unknown fallback recommendation method `%s`", recommender))
			return
		}
	}
	results, err := s.Recommend(userId, category, offset+n, recommenders...)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	results = results[mathutil.Min(offset, len(results)):]
	// write back
	if writeBackFeedback != "" {
		for _, itemId := range results {
			// insert to data store
			feedback := data.Feedback{
				FeedbackKey: data.FeedbackKey{
					UserId:       userId,
					ItemId:       itemId,
					FeedbackType: writeBackFeedback,
				},
				Timestamp: startTime.Add(writeBackDelay),
			}
			err = s.DataClient.BatchInsertFeedback([]data.Feedback{feedback}, false, false, false)
			if err != nil {
				InternalServerError(response, err)
				return
			}
			// insert to cache store
			err = s.InsertFeedbackToCache([]data.Feedback{feedback})
			if err != nil {
				InternalServerError(response, err)
				return
			}
		}
	}
	GetRecommendSeconds.Observe(time.Since(startTime).Seconds())
	// Send result
	Ok(response, results)
}

func (s *RestServer) sessionRecommend(request *restful.Request, response *restful.Response) {
	// parse arguments
	var feedbacks []Feedback
	if err := request.ReadEntity(&feedbacks); err != nil {
		BadRequest(response, err)
		return
	}
	n, err := ParseInt(request, "n", s.GorseConfig.Server.DefaultN)
	if err != nil {
		BadRequest(response, err)
		return
	}
	category := request.PathParameter("category")
	offset, err := ParseInt(request, "offset", 0)
	if err != nil {
		BadRequest(response, err)
		return
	}

	// pre-process feedback
	dataFeedback := make([]data.Feedback, len(feedbacks))
	for i := range dataFeedback {
		var err error
		dataFeedback[i], err = feedbacks[i].ToDataFeedback()
		if err != nil {
			BadRequest(response, err)
			return
		}
	}
	data.SortFeedbacks(dataFeedback)

	// item-based recommendation
	var excludeSet = strset.New()
	var userFeedback []data.Feedback
	for _, feedback := range dataFeedback {
		excludeSet.Add(feedback.ItemId)
		if s.GorseConfig.Recommend.Online.NumFeedbackFallbackItemBased <= len(userFeedback) {
			break
		}
		if funk.ContainsString(s.GorseConfig.Recommend.DataSource.PositiveFeedbackTypes, feedback.FeedbackType) {
			userFeedback = append(userFeedback, feedback)
		}
	}
	// collect candidates
	candidates := make(map[string]float64)
	for _, feedback := range userFeedback {
		// load similar items
		similarItems, err := s.CacheClient.GetSorted(cache.Key(cache.ItemNeighbors, feedback.ItemId, category), 0, s.GorseConfig.Recommend.CacheSize)
		if err != nil {
			BadRequest(response, err)
			return
		}
		// add unseen items
		similarItems = s.FilterOutHiddenScores(similarItems)
		for _, item := range similarItems {
			if !excludeSet.Has(item.Id) {
				candidates[item.Id] += item.Score
			}
		}
	}
	// collect top k
	filter := heap.NewTopKStringFilter(n + offset)
	for id, score := range candidates {
		filter.Push(id, score)
	}
	result := cache.CreateScoredItems(filter.PopAll())
	if len(result) > offset {
		result = result[offset:]
	} else {
		result = nil
	}
	result = result[:lo.Min([]int{len(result), n})]
	// Send result
	Ok(response, result)
}

// Success is the returned data structure for data insert operations.
type Success struct {
	RowAffected int
}

func (s *RestServer) insertUser(request *restful.Request, response *restful.Response) {
	temp := data.User{}
	// get userInfo from request and put into temp
	if err := request.ReadEntity(&temp); err != nil {
		BadRequest(response, err)
		return
	}
	if err := s.DataClient.BatchInsertUsers([]data.User{temp}); err != nil {
		InternalServerError(response, err)
		return
	}
	// insert modify timestamp
	if err := s.CacheClient.Set(cache.Time(cache.Key(cache.LastModifyUserTime, temp.UserId), time.Now())); err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, Success{RowAffected: 1})
}

func (s *RestServer) modifyUser(request *restful.Request, response *restful.Response) {
	// get user id
	userId := request.PathParameter("user-id")
	// modify user
	var patch data.UserPatch
	if err := request.ReadEntity(&patch); err != nil {
		BadRequest(response, err)
		return
	}
	if err := s.DataClient.ModifyUser(userId, patch); err != nil {
		InternalServerError(response, err)
		return
	}
	// insert modify timestamp
	if err := s.CacheClient.Set(cache.Time(cache.Key(cache.LastModifyUserTime, userId), time.Now())); err != nil {
		return
	}
	Ok(response, Success{RowAffected: 1})
}

func (s *RestServer) getUser(request *restful.Request, response *restful.Response) {
	// get user id
	userId := request.PathParameter("user-id")
	// get user
	user, err := s.DataClient.GetUser(userId)
	if err != nil {
		if errors.IsNotFound(err) {
			PageNotFound(response, err)
		} else {
			InternalServerError(response, err)
		}
		return
	}
	Ok(response, user)
}

func (s *RestServer) insertUsers(request *restful.Request, response *restful.Response) {
	var temp []data.User
	// get param from request and put into temp
	if err := request.ReadEntity(&temp); err != nil {
		BadRequest(response, err)
		return
	}
	// range temp and achieve user
	if err := s.DataClient.BatchInsertUsers(temp); err != nil {
		InternalServerError(response, err)
		return
	}
	// insert modify timestamp
	values := make([]cache.Value, len(temp))
	for i, user := range temp {
		values[i] = cache.Time(cache.Key(cache.LastModifyUserTime, user.UserId), time.Now())
	}
	if err := s.CacheClient.Set(values...); err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, Success{RowAffected: len(temp)})
}

type UserIterator struct {
	Cursor string
	Users  []data.User
}

func (s *RestServer) getUsers(request *restful.Request, response *restful.Response) {
	cursor := request.QueryParameter("cursor")
	n, err := ParseInt(request, "n", s.GorseConfig.Server.DefaultN)
	if err != nil {
		BadRequest(response, err)
		return
	}
	// get all users
	cursor, users, err := s.DataClient.GetUsers(cursor, n)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, UserIterator{Cursor: cursor, Users: users})
}

// delete a user by user-id
func (s *RestServer) deleteUser(request *restful.Request, response *restful.Response) {
	// get user-id and put into temp
	userId := request.PathParameter("user-id")
	if err := s.DataClient.DeleteUser(userId); err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, Success{RowAffected: 1})
}

// get feedback by user-id with feedback type
func (s *RestServer) getTypedFeedbackByUser(request *restful.Request, response *restful.Response) {
	feedbackType := request.PathParameter("feedback-type")
	userId := request.PathParameter("user-id")
	feedback, err := s.DataClient.GetUserFeedback(userId, false, feedbackType)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, feedback)
}

// get feedback by user-id
func (s *RestServer) getFeedbackByUser(request *restful.Request, response *restful.Response) {
	userId := request.PathParameter("user-id")
	feedback, err := s.DataClient.GetUserFeedback(userId, false)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, feedback)
}

// Item is the data structure for the item but stores the timestamp using string.
type Item struct {
	ItemId     string
	IsHidden   bool
	Categories []string
	Timestamp  string
	Labels     []string
	Comment    string
}

func (s *RestServer) batchInsertItems(response *restful.Response, temp []Item) {
	var count int
	items := make([]data.Item, 0, len(temp))
	timeScores := make(map[string][]cache.Scored)
	popularScores := make(map[string][]cache.Scored)
	var itemIds []string
	popularScore := lo.Map(temp, func(item Item, i int) float64 {
		return s.PopularItemsCache.GetSortedScore(item.ItemId)
	})
	for i, item := range temp {
		// parse datetime
		var timestamp time.Time
		var err error
		if item.Timestamp != "" {
			if timestamp, err = dateparse.ParseAny(item.Timestamp); err != nil {
				BadRequest(response, err)
				return
			}
		}
		items = append(items, data.Item{
			ItemId:     item.ItemId,
			IsHidden:   item.IsHidden,
			Categories: item.Categories,
			Timestamp:  timestamp,
			Labels:     item.Labels,
			Comment:    item.Comment,
		})
		for _, category := range append([]string{""}, item.Categories...) {
			timeScores[category] = append(timeScores[category], cache.Scored{
				Id:    item.ItemId,
				Score: float64(timestamp.Unix()),
			})
			if popularScore[i] > 0 {
				popularScores[category] = append(popularScores[category], cache.Scored{
					Id:    item.ItemId,
					Score: popularScore[i],
				})
			}
		}
		itemIds = append(itemIds, item.ItemId)
		count++
	}
	if err := s.deleteItemFromLatestPopularCache(itemIds, false); err != nil {
		InternalServerError(response, err)
		return
	}
	err := s.DataClient.BatchInsertItems(items)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	// insert modify timestamp and categories
	categories := strset.New()
	values := make([]cache.Value, len(items))
	for i, item := range items {
		values[i] = cache.Time(cache.Key(cache.LastModifyItemTime, item.ItemId), time.Now())
		categories.Add(item.Categories...)
	}
	if err = s.CacheClient.Set(values...); err != nil {
		InternalServerError(response, err)
		return
	}
	if err = s.CacheClient.AddSet(cache.ItemCategories, categories.List()...); err != nil {
		InternalServerError(response, err)
		return
	}
	// insert timestamp score and popular score
	sortedSets := make([]cache.SortedSet, 0, len(timeScores)+len(popularScores))
	for category, score := range timeScores {
		sortedSets = append(sortedSets, cache.Sorted(cache.Key(cache.LatestItems, category), score))
	}
	for category, score := range popularScores {
		sortedSets = append(sortedSets, cache.Sorted(cache.Key(cache.PopularItems, category), score))
	}
	if err = s.CacheClient.AddSorted(sortedSets...); err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, Success{RowAffected: count})
}

func (s *RestServer) insertItems(request *restful.Request, response *restful.Response) {
	var items []Item
	if err := request.ReadEntity(&items); err != nil {
		BadRequest(response, err)
		return
	}
	// Insert items
	s.batchInsertItems(response, items)
}

func (s *RestServer) insertItem(request *restful.Request, response *restful.Response) {
	var item Item
	var err error
	if err = request.ReadEntity(&item); err != nil {
		BadRequest(response, err)
		return
	}
	s.batchInsertItems(response, []Item{item})
}

func (s *RestServer) modifyItem(request *restful.Request, response *restful.Response) {
	// Get item id
	itemId := request.PathParameter("item-id")
	// modify item
	var patch data.ItemPatch
	if err := request.ReadEntity(&patch); err != nil {
		BadRequest(response, err)
		return
	}
	// refresh category cache
	if patch.Categories != nil {
		if err := s.deleteItemFromLatestPopularCache([]string{itemId}, false); err != nil {
			InternalServerError(response, err)
			return
		}
	}
	if err := s.DataClient.ModifyItem(itemId, patch); err != nil {
		InternalServerError(response, err)
		return
	}
	// insert hidden items to cache
	if patch.IsHidden != nil {
		var err error
		if *patch.IsHidden {
			err = s.CacheClient.AddSorted(cache.Sorted(cache.HiddenItemsV2, []cache.Scored{{itemId, float64(time.Now().Unix())}}))
		} else {
			err = s.CacheClient.RemSorted(cache.HiddenItemsV2, itemId)
		}
		if err != nil && !errors.IsNotFound(err) {
			InternalServerError(response, err)
			return
		}
	}
	// insert new timestamp to the latest scores
	if patch.Timestamp != nil || patch.Categories != nil {
		item, err := s.DataClient.GetItem(itemId)
		if err != nil {
			InternalServerError(response, err)
			return
		}
		popularScore := s.PopularItemsCache.GetSortedScore(itemId)
		var sortedSets []cache.SortedSet
		for _, category := range append([]string{""}, item.Categories...) {
			sortedSets = append(sortedSets, cache.Sorted(cache.Key(cache.LatestItems, category), []cache.Scored{{Id: itemId, Score: float64(item.Timestamp.Unix())}}))
			if popularScore > 0 {
				sortedSets = append(sortedSets, cache.Sorted(cache.Key(cache.PopularItems, category), []cache.Scored{{Id: itemId, Score: popularScore}}))
			}
		}
		if err = s.CacheClient.AddSorted(sortedSets...); err != nil {
			InternalServerError(response, err)
			return
		}
	}
	// insert modify timestamp
	if err := s.CacheClient.Set(cache.Time(cache.Key(cache.LastModifyItemTime, itemId), time.Now())); err != nil {
		return
	}
	Ok(response, Success{RowAffected: 1})
}

// ItemIterator is the iterator for items.
type ItemIterator struct {
	Cursor string
	Items  []data.Item
}

func (s *RestServer) getItems(request *restful.Request, response *restful.Response) {
	cursor := request.QueryParameter("cursor")
	n, err := ParseInt(request, "n", s.GorseConfig.Server.DefaultN)
	if err != nil {
		BadRequest(response, err)
		return
	}
	cursor, items, err := s.DataClient.GetItems(cursor, n, nil)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, ItemIterator{Cursor: cursor, Items: items})
}

func (s *RestServer) getItem(request *restful.Request, response *restful.Response) {
	// Get item id
	itemId := request.PathParameter("item-id")
	// Get item
	item, err := s.DataClient.GetItem(itemId)
	if err != nil {
		if errors.IsNotFound(err) {
			PageNotFound(response, err)
		} else {
			InternalServerError(response, err)
		}
		return
	}
	Ok(response, item)
}

func (s *RestServer) deleteItem(request *restful.Request, response *restful.Response) {
	itemId := request.PathParameter("item-id")
	// delete items from latest and popular
	if err := s.deleteItemFromLatestPopularCache([]string{itemId}, true); err != nil {
		InternalServerError(response, err)
		return
	}
	if err := s.DataClient.DeleteItem(itemId); err != nil {
		InternalServerError(response, err)
		return
	}
	// insert deleted item to cache
	if err := s.CacheClient.AddSorted(cache.Sorted(cache.HiddenItemsV2, []cache.Scored{{itemId, float64(time.Now().Unix())}})); err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, Success{RowAffected: 1})
}

func (s *RestServer) deleteItemFromLatestPopularCache(itemIds []string, deleteItem bool) error {
	var deleteKeys []string
	if deleteItem {
		deleteKeys = []string{cache.LatestItems, cache.PopularItems}
	}
	if items, err := s.DataClient.BatchGetItems(itemIds); err != nil {
		if errors.IsNotFound(err) {
			// do nothing if the item doesn't exist
			return nil
		} else {
			return err
		}
	} else {
		for _, item := range items {
			for _, category := range item.Categories {
				deleteKeys = append(deleteKeys, cache.Key(cache.LatestItems, category))
				deleteKeys = append(deleteKeys, cache.Key(cache.PopularItems, category))
			}
			for _, deleteKey := range deleteKeys {
				if err = s.CacheClient.RemSorted(deleteKey, item.ItemId); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *RestServer) insertItemCategory(request *restful.Request, response *restful.Response) {
	// Get item id and category
	itemId := request.PathParameter("item-id")
	category := request.PathParameter("category")
	// Insert category
	item, err := s.DataClient.GetItem(itemId)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	if !funk.ContainsString(item.Categories, category) {
		item.Categories = append(item.Categories, category)
	}
	err = s.DataClient.BatchInsertItems([]data.Item{item})
	if err != nil {
		InternalServerError(response, err)
		return
	}
	// insert to popular
	popularScore := s.PopularItemsCache.GetSortedScore(itemId)
	if popularScore > 0 {
		if err = s.CacheClient.AddSorted(cache.Sorted(cache.Key(cache.PopularItems, category), []cache.Scored{{Id: itemId, Score: popularScore}})); err != nil {
			InternalServerError(response, err)
			return
		}
	}
	// insert item to latest
	if err = s.CacheClient.AddSorted(cache.Sorted(cache.Key(cache.LatestItems, category), []cache.Scored{{Id: itemId, Score: float64(item.Timestamp.Unix())}})); err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, Success{RowAffected: 1})
}

func (s *RestServer) deleteItemCategory(request *restful.Request, response *restful.Response) {
	// Get item id and category
	itemId := request.PathParameter("item-id")
	category := request.PathParameter("category")
	// Delete category
	item, err := s.DataClient.GetItem(itemId)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	categories := make([]string, 0, len(item.Categories))
	for _, cat := range item.Categories {
		if cat != category {
			categories = append(categories, cat)
		}
	}
	item.Categories = categories
	err = s.DataClient.BatchInsertItems([]data.Item{item})
	if err != nil {
		InternalServerError(response, err)
		return
	}
	// remove item from popular
	if err = s.CacheClient.RemSorted(cache.Key(cache.PopularItems, category), itemId); err != nil {
		InternalServerError(response, err)
		return
	}
	// remove item from latest
	if err = s.CacheClient.RemSorted(cache.Key(cache.LatestItems, category), itemId); err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, Success{RowAffected: 1})
}

// Feedback is the data structure for the feedback but stores the timestamp using string.
type Feedback struct {
	data.FeedbackKey
	Timestamp string
	Comment   string
}

func (f Feedback) ToDataFeedback() (data.Feedback, error) {
	var feedback data.Feedback
	feedback.FeedbackKey = f.FeedbackKey
	feedback.Comment = f.Comment
	if f.Timestamp != "" {
		var err error
		feedback.Timestamp, err = dateparse.ParseAny(f.Timestamp)
		if err != nil {
			return data.Feedback{}, err
		}
	}
	return feedback, nil
}

func (s *RestServer) insertFeedback(overwrite bool) func(request *restful.Request, response *restful.Response) {
	return func(request *restful.Request, response *restful.Response) {
		// add ratings
		var feedbackLiterTime []Feedback
		if err := request.ReadEntity(&feedbackLiterTime); err != nil {
			BadRequest(response, err)
			return
		}
		// parse datetime
		var err error
		feedback := make([]data.Feedback, len(feedbackLiterTime))
		users := set.NewStringSet()
		items := set.NewStringSet()
		for i := range feedback {
			users.Add(feedbackLiterTime[i].UserId)
			items.Add(feedbackLiterTime[i].ItemId)
			feedback[i], err = feedbackLiterTime[i].ToDataFeedback()
			if err != nil {
				BadRequest(response, err)
				return
			}
		}
		// insert feedback to data store
		err = s.DataClient.BatchInsertFeedback(feedback,
			s.GorseConfig.Server.AutoInsertUser,
			s.GorseConfig.Server.AutoInsertItem, overwrite)
		if err != nil {
			InternalServerError(response, err)
			return
		}
		// insert feedback to cache store
		if err = s.InsertFeedbackToCache(feedback); err != nil {
			InternalServerError(response, err)
			return
		}
		values := make([]cache.Value, 0, users.Size()+items.Size())
		for _, userId := range users.List() {
			values = append(values, cache.Time(cache.Key(cache.LastModifyUserTime, userId), time.Now()))
		}
		for _, itemId := range items.List() {
			values = append(values, cache.Time(cache.Key(cache.LastModifyItemTime, itemId), time.Now()))
		}
		if err = s.CacheClient.Set(values...); err != nil {
			InternalServerError(response, err)
			return
		}
		Ok(response, Success{RowAffected: len(feedback)})
	}
}

// FeedbackIterator is the iterator for feedback.
type FeedbackIterator struct {
	Cursor   string
	Feedback []data.Feedback
}

func (s *RestServer) getFeedback(request *restful.Request, response *restful.Response) {
	// Parse parameters
	cursor := request.QueryParameter("cursor")
	n, err := ParseInt(request, "n", s.GorseConfig.Server.DefaultN)
	if err != nil {
		BadRequest(response, err)
		return
	}
	cursor, feedback, err := s.DataClient.GetFeedback(cursor, n, nil)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, FeedbackIterator{Cursor: cursor, Feedback: feedback})
}

func (s *RestServer) getTypedFeedback(request *restful.Request, response *restful.Response) {
	// Parse parameters
	feedbackType := request.PathParameter("feedback-type")
	cursor := request.QueryParameter("cursor")
	n, err := ParseInt(request, "n", s.GorseConfig.Server.DefaultN)
	if err != nil {
		BadRequest(response, err)
		return
	}
	cursor, feedback, err := s.DataClient.GetFeedback(cursor, n, nil, feedbackType)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, FeedbackIterator{Cursor: cursor, Feedback: feedback})
}

func (s *RestServer) getUserItemFeedback(request *restful.Request, response *restful.Response) {
	// Parse parameters
	userId := request.PathParameter("user-id")
	itemId := request.PathParameter("item-id")
	if feedback, err := s.DataClient.GetUserItemFeedback(userId, itemId); err != nil {
		InternalServerError(response, err)
	} else {
		Ok(response, feedback)
	}
}

func (s *RestServer) deleteUserItemFeedback(request *restful.Request, response *restful.Response) {
	// Parse parameters
	userId := request.PathParameter("user-id")
	itemId := request.PathParameter("item-id")
	if deleteCount, err := s.DataClient.DeleteUserItemFeedback(userId, itemId); err != nil {
		InternalServerError(response, err)
	} else {
		Ok(response, Success{RowAffected: deleteCount})
	}
}

func (s *RestServer) getTypedUserItemFeedback(request *restful.Request, response *restful.Response) {
	// Parse parameters
	feedbackType := request.PathParameter("feedback-type")
	userId := request.PathParameter("user-id")
	itemId := request.PathParameter("item-id")
	if feedback, err := s.DataClient.GetUserItemFeedback(userId, itemId, feedbackType); err != nil {
		InternalServerError(response, err)
	} else if feedbackType == "" {
		Text(response, "{}")
	} else {
		Ok(response, feedback[0])
	}
}

func (s *RestServer) deleteTypedUserItemFeedback(request *restful.Request, response *restful.Response) {
	// Parse parameters
	feedbackType := request.PathParameter("feedback-type")
	userId := request.PathParameter("user-id")
	itemId := request.PathParameter("item-id")
	if deleteCount, err := s.DataClient.DeleteUserItemFeedback(userId, itemId, feedbackType); err != nil {
		InternalServerError(response, err)
	} else {
		Ok(response, Success{deleteCount})
	}
}

func (s *RestServer) getMeasurements(request *restful.Request, response *restful.Response) {
	// Parse parameters
	name := request.PathParameter("name")
	n, err := ParseInt(request, "n", 100)
	if err != nil {
		BadRequest(response, err)
		return
	}
	measurements, err := s.DataClient.GetMeasurements(name, n)
	if err != nil {
		InternalServerError(response, err)
		return
	}
	Ok(response, measurements)
}

// BadRequest returns a bad request error.
func BadRequest(response *restful.Response, err error) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	base.Logger().Error("bad request", zap.Error(err))
	if err = response.WriteError(http.StatusBadRequest, err); err != nil {
		base.Logger().Error("failed to write error", zap.Error(err))
	}
}

// InternalServerError returns a internal server error.
func InternalServerError(response *restful.Response, err error) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	base.Logger().Error("internal server error", zap.Error(err))
	if err = response.WriteError(http.StatusInternalServerError, err); err != nil {
		base.Logger().Error("failed to write error", zap.Error(err))
	}
}

// PageNotFound returns a not found error.
func PageNotFound(response *restful.Response, err error) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	if err := response.WriteError(http.StatusNotFound, err); err != nil {
		base.Logger().Error("failed to write error", zap.Error(err))
	}
}

// Ok sends the content as JSON to the client.
func Ok(response *restful.Response, content interface{}) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	if err := response.WriteAsJson(content); err != nil {
		base.Logger().Error("failed to write json", zap.Error(err))
	}
}

// Text returns a plain text.
func Text(response *restful.Response, content string) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	if _, err := response.Write([]byte(content)); err != nil {
		base.Logger().Error("failed to write text", zap.Error(err))
	}
}

// InsertFeedbackToCache inserts feedback to cache.
func (s *RestServer) InsertFeedbackToCache(feedback []data.Feedback) error {
	if !s.GorseConfig.Recommend.Replacement.EnableReplacement {
		sortedSets := make([]cache.SortedSet, len(feedback))
		for i, v := range feedback {
			sortedSets[i] = cache.Sorted(cache.Key(cache.IgnoreItems, v.UserId), []cache.Scored{{Id: v.ItemId, Score: float64(v.Timestamp.Unix())}})
		}
		if err := s.CacheClient.AddSorted(sortedSets...); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}
