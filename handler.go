package cache

import (
	"fmt"
	"github.com/mholt/caddy/caddyhttp/httpserver"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"
)

type CacheHandler struct {
	Config *Config
	Cache  *Cache
	Next   httpserver.Handler
}

/**
 * Builds the cache key
 */
func getKey(r *http.Request) string {
	key := r.Method + " " + r.Host + r.URL.Path

	q := r.URL.Query().Encode()
	if len(q) > 0 {
		key += "?" + q
	}

	return key
}

/**
 * Returns a function that given a previous response returns if it matches the current response
 */
func matchesRequest(r *http.Request) func(*HttpCacheEntry) bool {
	return func(entry *HttpCacheEntry) bool {
		// TODO match getKeys()
		// It is always called with same key values
		// But checking it is better
		vary, hasVary := entry.Response.HeaderMap["Vary"]
		if !hasVary {
			return true
		}

		for _, searchedHeader := range strings.Split(vary[0], ",") {
			searchedHeader = strings.TrimSpace(searchedHeader)
			if !reflect.DeepEqual(entry.Request.HeaderMap[searchedHeader], r.Header[searchedHeader]) {
				return false
			}
		}
		return true
	}
}

func (h *CacheHandler) AddStatusHeaderIfConfigured(w http.ResponseWriter, status string) {
	if h.Config.StatusHeader != "" {
		w.Header().Set(h.Config.StatusHeader, status)
	}
}

/**
* This prevents storing status header in cache.
* Otherwise the status cache will be sent twice for cached results
 */
func (h *CacheHandler) RemoveStatusHeaderIfConfigured(headers http.Header) http.Header {
	if h.Config.StatusHeader != "" {
		delete(headers, h.Config.StatusHeader)
	}
	return headers
}

func (handler *CacheHandler) HandleCachedResponse(w http.ResponseWriter, r *http.Request, previous *HttpCacheEntry) int {
	handler.AddStatusHeaderIfConfigured(w, "hit")
	for k, values := range previous.Response.HeaderMap {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(previous.Response.Code)
	if previous.Response.Body != nil {
		io.Copy(w, previous.Response.Body.GetReader())
	}
	return previous.Response.Code
}

func (handler *CacheHandler) HandleNonCachedResponse(w http.ResponseWriter, r *http.Request) (*HttpCacheEntry, chan struct{}, error) {
	handler.AddStatusHeaderIfConfigured(w, "miss")

	entry := &HttpCacheEntry{
		isPublic:   false, // Default values for private responses
		Expiration: time.Now().UTC().Add(time.Duration(1) * time.Hour),
		Request:    &Request{HeaderMap: r.Header},
	}
	endChannel := make(chan struct{})

	pipe := PipeHandlerToChannels(handler.Next, w, r)

	// Fetch from upstream in another thread
	go func() {
		//fmt.Println("Handling")
		pipe.handle()
		//fmt.Println("Ended pipe.handle()")
		endChannel <- struct{}{}
	}()

	// Wait until headers
	headers := <-pipe.HeaderChannel()

	// Check if the request is cacheable
	isCacheable, expirationTime, err := getCacheableStatus(r, headers.StatusCode, *headers.Header, handler.Config)
	if err != nil {
		fmt.Println(err)
		return nil, endChannel, err
	}

	if !isCacheable {
		//fmt.Println("Not cacheable")
		entry.isPublic = false
		entry.Response = &Response{
			HeaderMap: handler.RemoveStatusHeaderIfConfigured(*headers.Header),
			Code:      headers.StatusCode,
		}
		go func() {
			for range pipe.BodyChannel() {
			}
		}()
		return entry, endChannel, nil
	}

	// if it is create a new content writer
	bodyWriter, err := handler.Cache.NewContent(getKey(r))

	if err != nil {
		fmt.Println(err)
		return nil, endChannel, err
	}

	// Set entry fields
	entry.Expiration = expirationTime
	entry.isPublic = true
	entry.Response = &Response{
		HeaderMap: handler.RemoveStatusHeaderIfConfigured(*headers.Header),
		Body:      bodyWriter,
		Code:      headers.StatusCode,
	}

	// Create a new thread that will save the fetched bytes to the content
	go func() {
		for content := range pipe.BodyChannel() {
			bodyWriter.Write(content)
		}
		bodyWriter.Close()
	}()

	return entry, endChannel, nil
}

func (handler CacheHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	if !shouldUseCache(r) {
		handler.AddStatusHeaderIfConfigured(w, "skip")
		return handler.Next.ServeHTTP(w, r)
	}

	var endChannel chan struct{}
	returnedStatusCode := http.StatusInternalServerError // If this is not updated means there was an error
	err := handler.Cache.GetOrSet(getKey(r), matchesRequest(r), func(previous *HttpCacheEntry) (*HttpCacheEntry, error) {
		if previous == nil || !previous.isPublic {
			//fmt.Println("GetOrSet, isNil? =", previous == nil)
			newEntry, endChn, err := handler.HandleNonCachedResponse(w, r)
			if err != nil {
				return nil, err
			}
			endChannel = endChn
			returnedStatusCode = newEntry.Response.Code
			return newEntry, nil
		}

		handler.AddStatusHeaderIfConfigured(w, "hit")
		returnedStatusCode = handler.HandleCachedResponse(w, r, previous)
		return nil, nil
	})
	//fmt.Println("Sale del getOrSet y err=", err)
	if endChannel != nil {
		//fmt.Println("Esperando endChannel")
		<-endChannel
		//fmt.Println("endChannel called")
	} else {
		//fmt.Println("endChannel is nil !!!! Wrong !")
	}
	//fmt.Println(err)
	return returnedStatusCode, err
}
