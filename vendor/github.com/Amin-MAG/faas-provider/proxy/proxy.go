// Package proxy provides a default function invocation proxy method for OpenFaaS providers.
//
// The function proxy logic is used by the Gateway when `direct_functions` is set to false.
// This means that the provider will direct call the function and return the results.  This
// involves resolving the function by name and then copying the result into the original HTTP
// request.
//
// openfaas-provider has implemented a standard HTTP HandlerFunc that will handle setting
// timeout values, parsing the request path, and copying the request/response correctly.
//
//	bootstrapHandlers := bootTypes.FaaSHandlers{
//		FunctionProxy:  proxy.NewHandlerFunc(timeout, resolver),
//		DeleteHandler:  handlers.MakeDeleteHandler(clientset),
//		DeployHandler:  handlers.MakeDeployHandler(clientset),
//		FunctionReader: handlers.MakeFunctionReader(clientset),
//		ReplicaReader:  handlers.MakeReplicaReader(clientset),
//		ReplicaUpdater: handlers.MakeReplicaUpdater(clientset),
//		InfoHandler:    handlers.MakeInfoHandler(),
//	}
//
// proxy.NewHandlerFunc is optional, but does simplify the logic of your provider.
package proxy

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Amin-MAG/faas-provider/httputil"
	"github.com/Amin-MAG/faas-provider/types"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

const (
	watchdogPort       = "8080"
	defaultContentType = "text/plain"
)

// BaseURLResolver URL resolver for proxy requests
//
// The FaaS provider implementation is responsible for providing the resolver function implementation.
// BaseURLResolver.Resolve will receive the function name and should return the URL of the
// function service.
type BaseURLResolver interface {
	Resolve(functionName string) (url.URL, error)
}

// NewHandlerFunc creates a standard http.HandlerFunc to proxy function requests.
// The returned http.HandlerFunc will ensure:
//
//   - proper proxy request timeouts
//   - proxy requests for GET, POST, PATCH, PUT, and DELETE
//   - path parsing including support for extracing the function name, sub-paths, and query paremeters
//   - passing and setting the `X-Forwarded-Host` and `X-Forwarded-For` headers
//   - logging errors and proxy request timing to stdout
//
// Note that this will panic if `resolver` is nil.
func NewHandlerFunc(config types.FaaSConfig, resolver BaseURLResolver) http.HandlerFunc {
	if resolver == nil {
		panic("NewHandlerFunc: empty proxy handler resolver, cannot be nil")
	}

	proxyClient := NewProxyClientFromConfig(config)

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			defer r.Body.Close()
		}

		switch r.Method {
		case http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
			http.MethodGet,
			http.MethodOptions,
			http.MethodHead:
			proxyRequest(w, r, proxyClient, resolver)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func NewFlowHandler(config types.FaaSConfig, redisClient *redis.Client, resolver BaseURLResolver, flows types.Flows) http.HandlerFunc {
	if resolver == nil {
		panic("NewFlowHandler: empty proxy handler resolver, cannot be nil")
	}

	proxyClient := NewProxyClientFromConfig(config)

	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("the flow proxy handler is working properly.")
		fmt.Printf("body is: %+v\n", r.Body)

		// Fetch the name of the node (function)
		pathVars := mux.Vars(r)
		functionName := pathVars["name"]
		if functionName == "" {
			httputil.Errorf(w, http.StatusBadRequest, "Provide function name in the request path")
			return
		}

		// Fetch the flow of the node (function)
		flow, ok := flows.Flows[functionName]
		if !ok {
			httputil.Errorf(w, http.StatusBadRequest, "Can not find this kind of function in flows config")
		}

		// Read current request body
		var requestBody map[string]interface{}
		err := json.NewDecoder(r.Body).Decode(&requestBody)
		if err != nil {
			log.Println("Error decoding JSON request body:", err)
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		// TODO: Check required fields for args

		// Initialize the input flow variable
		flowInput := types.FlowInput{
			Args:     requestBody,
			Children: make(map[string]*types.FlowOutput),
		}

		// Try caching if it is enabled for function
		if config.EnableCaching && flow.Caching {
			reqBody, err := json.Marshal(requestBody)
			if err != nil {
				fmt.Printf("error in marshalling args of function %s for caching: %s\n", functionName, err.Error())
			}

			// Generate SHA1 hash of the JSON string
			hashBytes := sha1.Sum(reqBody)
			hashString := fmt.Sprintf("%x", hashBytes)

			// Try to get cached response from Redis
			cachedResponseBytes, err := redisClient.Get(r.Context(), hashString).Bytes()
			if err == nil {
				// If cached response exists, parse the JSON string and return it
				w.WriteHeader(http.StatusOK)
				_, _ = io.Copy(w, bytes.NewBuffer(cachedResponseBytes))
				return
			}
		}

		// Iterate the children of the node (function)
		for alias, child := range flow.Children {
			// Recursively run the flow for these nodes
			fmt.Printf("processing the %s: [%+v], a child of %s\n", alias, child, functionName)

			// Grabbing the arguments of child
			args := make(map[string]interface{})
			for argField, mapField := range child.ArgsMap {
				args[argField] = flowInput.Args[mapField]
			}

			// Proxy the child function
			childRequestBody, err := json.Marshal(args)
			if err != nil {
				fmt.Printf("error in marshalling args of function %s: %s\n", alias, err.Error())
			}
			req, err := http.NewRequest(
				"POST",
				fmt.Sprintf("http://127.0.0.1:8081/flow/%s", child.Function),
				bytes.NewBuffer(childRequestBody),
			)
			if err != nil {
				fmt.Printf("error in creating new request of function %s: %s\n", alias, err.Error())
			}
			req.Header.Set("Content-Type", "application/json")

			// Do the request
			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("error in doing the request of function %s: %s\n", alias, err.Error())
			}

			// Read the response body
			data := make(map[string]interface{})
			childResponseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Printf("error in reading the response of function %s: %s\n", alias, err.Error())
			}

			fmt.Println(string(childResponseBody))
			err = json.Unmarshal(childResponseBody, &data)
			if err != nil {
				fmt.Printf("error in unmarshalling the response of function %s: %s\n", alias, err.Error())
			}

			// Save the responses
			flowInput.Children[alias] = &types.FlowOutput{
				Data:     data,
				Function: child.Function,
			}
		}
		fmt.Printf("the flow input of the %s is: %+v\n", functionName, flowInput)

		// Create a new request body
		newRequestBody, _ := json.Marshal(flowInput)
		fmt.Printf("new body request of %s is: %+v\n", functionName, newRequestBody)

		// Replace the existing request body with the new one
		r.Body = io.NopCloser(bytes.NewBuffer(newRequestBody))

		// Execute the target flow
		proxyRequest(w, r, proxyClient, resolver)

		// Cache the response of the function
		if config.EnableCaching && flow.Caching {
			reqBody, err := json.Marshal(flowInput.Args)
			if err != nil {
				fmt.Printf("error in marshalling args of function %s for caching: %s\n", functionName, err.Error())
			}

			// Generate SHA1 hash of the JSON string
			hashBytes := sha1.Sum(reqBody)
			hashString := fmt.Sprintf("%x", hashBytes)

			// Save the response
			responseBytes, _ := io.ReadAll(r.Body)
			redisClient.SetEx(r.Context(), hashString, responseBytes, time.Duration(flow.CacheTTL)*time.Second)
		}
	}
}

// NewProxyClientFromConfig creates a new http.Client designed for proxying requests and enforcing
// certain minimum configuration values.
func NewProxyClientFromConfig(config types.FaaSConfig) *http.Client {
	return NewProxyClient(config.GetReadTimeout(), config.GetMaxIdleConns(), config.GetMaxIdleConnsPerHost())
}

// NewProxyClient creates a new http.Client designed for proxying requests, this is exposed as a
// convenience method for internal or advanced uses. Most people should use NewProxyClientFromConfig.
func NewProxyClient(timeout time.Duration, maxIdleConns int, maxIdleConnsPerHost int) *http.Client {
	return &http.Client{
		// these Transport values ensure that the http Client will eventually timeout and prevents
		// infinite retries. The default http.Client configure these timeouts.  The specific
		// values tuned via performance testing/benchmarking
		//
		// Additional context can be found at
		// - https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
		// - https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
		//
		// Additionally, these overrides for the default client enable re-use of connections and prevent
		// CoreDNS from rate limiting under high traffic
		//
		// See also two similar projects where this value was updated:
		// https://github.com/prometheus/prometheus/pull/3592
		// https://github.com/minio/minio/pull/5860
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 1 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       120 * time.Millisecond,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1500 * time.Millisecond,
		},
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// proxyRequest handles the actual resolution of and then request to the function service.
func proxyRequest(w http.ResponseWriter, originalReq *http.Request, proxyClient *http.Client, resolver BaseURLResolver) {
	ctx := originalReq.Context()

	pathVars := mux.Vars(originalReq)
	functionName := pathVars["name"]
	if functionName == "" {
		httputil.Errorf(w, http.StatusBadRequest, "Provide function name in the request path")
		return
	}

	functionAddr, resolveErr := resolver.Resolve(functionName)
	if resolveErr != nil {
		// TODO: Should record the 404/not found error in Prometheus.
		log.Printf("resolver error: no endpoints for %s: %s\n", functionName, resolveErr.Error())
		httputil.Errorf(w, http.StatusServiceUnavailable, "No endpoints available for: %s.", functionName)
		return
	}

	proxyReq, err := buildProxyRequest(originalReq, functionAddr, pathVars["params"])
	if err != nil {
		httputil.Errorf(w, http.StatusInternalServerError, "Failed to resolve service: %s.", functionName)
		return
	}

	if proxyReq.Body != nil {
		defer proxyReq.Body.Close()
	}

	start := time.Now()
	response, err := proxyClient.Do(proxyReq.WithContext(ctx))
	seconds := time.Since(start)

	if err != nil {
		log.Printf("error with proxy request to: %s, %s\n", proxyReq.URL.String(), err.Error())

		httputil.Errorf(w, http.StatusInternalServerError, "Can't reach service for: %s.", functionName)
		return
	}

	if response.Body != nil {
		defer response.Body.Close()
	}

	log.Printf("%s took %f seconds\n", functionName, seconds.Seconds())

	clientHeader := w.Header()
	copyHeaders(clientHeader, &response.Header)
	w.Header().Set("Content-Type", getContentType(originalReq.Header, response.Header))

	w.WriteHeader(response.StatusCode)
	if response.Body != nil {
		io.Copy(w, response.Body)
	}
}

// buildProxyRequest creates a request object for the proxy request, it will ensure that
// the original request headers are preserved as well as setting openfaas system headers
func buildProxyRequest(originalReq *http.Request, baseURL url.URL, extraPath string) (*http.Request, error) {

	host := baseURL.Host
	if baseURL.Port() == "" {
		host = baseURL.Host + ":" + watchdogPort
	}

	url := url.URL{
		Scheme:   baseURL.Scheme,
		Host:     host,
		Path:     extraPath,
		RawQuery: originalReq.URL.RawQuery,
	}

	upstreamReq, err := http.NewRequest(originalReq.Method, url.String(), nil)
	if err != nil {
		return nil, err
	}
	copyHeaders(upstreamReq.Header, &originalReq.Header)

	if len(originalReq.Host) > 0 && upstreamReq.Header.Get("X-Forwarded-Host") == "" {
		upstreamReq.Header["X-Forwarded-Host"] = []string{originalReq.Host}
	}
	if upstreamReq.Header.Get("X-Forwarded-For") == "" {
		upstreamReq.Header["X-Forwarded-For"] = []string{originalReq.RemoteAddr}
	}

	if originalReq.Body != nil {
		upstreamReq.Body = originalReq.Body
	}

	return upstreamReq, nil
}

// copyHeaders clones the header values from the source into the destination.
func copyHeaders(destination http.Header, source *http.Header) {
	for k, v := range *source {
		vClone := make([]string, len(v))
		copy(vClone, v)
		destination[k] = vClone
	}
}

// getContentType resolves the correct Content-Type for a proxied function.
func getContentType(request http.Header, proxyResponse http.Header) (headerContentType string) {
	responseHeader := proxyResponse.Get("Content-Type")
	requestHeader := request.Get("Content-Type")

	if len(responseHeader) > 0 {
		headerContentType = responseHeader
	} else if len(requestHeader) > 0 {
		headerContentType = requestHeader
	} else {
		headerContentType = defaultContentType
	}

	return headerContentType
}
