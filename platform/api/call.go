// Copyright 2023 Cisco Systems, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package api provides access to the platform API, in all forms supported
// by the config context (aka access profile)
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/apex/log"
	"github.com/moul/http2curl"

	"github.com/cisco-open/fsoc/config"
)

var FlagCurlifyRequests bool

// --- Public Interface -----------------------------------------------------

// Options contains extra, optional parameteres that modify the API call behavior
type Options struct {
	// Headers contains additional headers to be provided in the request
	Headers map[string]string

	// ResponseHeaders will be populated with the headers returned by the call
	ResponseHeaders map[string][]string

	// ExpectedErrors is a list of status codes that are expected and should be logged as Info rather than Error
	ExpectedErrors []int

	// Quiet suppresses the interactive spinner and display of request
	Quiet bool

	// Context provides a Go context for the API call (nil is accepted and will be replaced with a default context)
	Context context.Context
}

// JSONGet performs a GET request and parses the response as JSON
func JSONGet(path string, out any, options *Options) error {
	return httpRequest("GET", path, nil, out, options)
}

// JSONDelete performs a DELETE request and parses the response as JSON
func JSONDelete(path string, out any, options *Options) error {
	return httpRequest("DELETE", path, nil, out, options)
}

// JSONPost performs a POST request with JSON command and response
func JSONPost(path string, body any, out any, options *Options) error {
	return httpRequest("POST", path, body, out, options)
}

// HTTPPost performs a POST request with HTTP command and response - Accept and Content-Type headers are provided by the caller
func HTTPPost(path string, body []byte, out any, options *Options) error {
	return httpRequest("POST", path, body, out, options)
}

// HTTPGet performs a GET request with HTTP command and response - Accept and Content-Type headers are provided by the caller
func HTTPGet(path string, out any, options *Options) error {
	return httpRequest("GET", path, nil, out, options)
}

// JSONPut performs a PUT request with JSON command and response
func JSONPut(path string, body any, out any, options *Options) error {
	return httpRequest("PUT", path, body, out, options)
}

// JSONPatch performs a PATCH request and parses the response as JSON
func JSONPatch(path string, body any, out any, options *Options) error {
	return httpRequest("PATCH", path, body, out, options)
}

// JSONRequest performs an HTTP request and parses the response as JSON, allowing
// the http method to be specified
func JSONRequest(method string, path string, body any, out any, options *Options) error {
	return httpRequest(method, path, body, out, options)
}

// --- Internal methods -----------------------------------------------------

func prepareHTTPRequest(ctx *callContext, client *http.Client, method string, path string, body any, headers map[string]string) (*http.Request, error) {
	// body will be JSONified if a body is given but no Content-Type is provided
	// (if a content type is provided, we assume the body is in the desired format)
	jsonify := body != nil && (headers == nil || headers["Content-Type"] == "")
	// Due to issues with encoding the special characters used for ids generated with identifyingProperties, we
	// need to create the fullPath for the request ourselves instead of calling uri.String()
	var fullPath string

	cfg := ctx.cfg // quick access

	// prepare a body reader
	var bodyReader io.Reader = nil
	if jsonify {
		// marshal body data to JSON
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal body data: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	} else if body != nil {
		// provide body data as a io.Reader
		bodyBytes, ok := body.([]byte)
		if !ok {
			return nil, fmt.Errorf("(bug) HTTP request body type must be []byte if Content-Type is provided, found %T instead", body)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	// create HTTP request
	path, query, _ := strings.Cut(path, "?")
	uri, err := url.Parse(cfg.URL)
	if err != nil {
		log.Fatalf("Failed to parse the url provided in context (%q): %v", cfg.URL, err)
	}
	// Create the full path again ensuring that we aren't double escaping characters in the path
	joinedPath, err := url.JoinPath(uri.String(), path)
	if err != nil {
		return nil, fmt.Errorf("failed to create a request for %q: %w", uri.String(), err)
	}

	if query == "" {
		fullPath = joinedPath
	} else {
		fullPath = fmt.Sprintf("%s?%s", joinedPath, query)
	}

	req, err := http.NewRequestWithContext(ctx.goContext, method, fullPath, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create a request for %q: %w", uri.String(), err)
	}

	// add headers that are not already provided
	if jsonify {
		contentType := "application/json"
		if method == "PATCH" {
			contentType = "application/merge-patch+json"
		}
		req.Header.Add("Content-Type", contentType)
	}
	if headers == nil || headers["Accept"] == "" {
		req.Header.Add("Accept", "application/json")
	}
	if headers == nil || headers["Authorization"] == "" {
		req.Header.Add("Authorization", "Bearer "+cfg.Token)
	}

	if cfg.AuthMethod == config.AuthMethodLocal {
		AddLocalAuthReqHeaders(req, &cfg.LocalAuthOptions)
	}

	// add explicit headers
	for k, v := range headers {
		req.Header.Add(k, v)
	}

	if FlagCurlifyRequests { // global --curl flag
		curlCommand, err := getCurlCommandOfRequest(req)
		if err != nil {
			return nil, fmt.Errorf("failed to generate curl equivalent command: %s", err)
		}
		log.WithField("command", curlCommand).Info("curl command equivalent")
	}

	return req, nil
}

func getCurlCommandOfRequest(req *http.Request) (string, error) {
	reqClone := req.Clone(context.Background())
	reqClone.Header.Set("Authorization", "Bearer REDACTED")

	if strings.HasPrefix(reqClone.Header.Get("Content-Type"), "multipart/form-data") {
		reqClone.Body = io.NopCloser(strings.NewReader("@/file/path/REDACTED"))
	} else if req.Body != nil {
		buf := &bytes.Buffer{}
		teeReader := io.TeeReader(req.Body, buf)
		b, err := io.ReadAll(teeReader)
		if err != nil {
			return "", err
		}
		req.Body = io.NopCloser(bytes.NewReader(b))
		reqClone.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	}
	command, _ := http2curl.GetCurlCommand(reqClone)
	return command.String(), nil
}

func httpRequest(method string, path string, body any, out any, options *Options) error {
	log.WithFields(log.Fields{"method": method, "path": path}).Info("Calling the observability platform API")

	// create a default options to avoid nil-checking
	if options == nil {
		options = &Options{}
	}

	callCtx := newCallContext(options.Context, options.Quiet)
	defer callCtx.stopSpinner(false) // ensure the spinner is not running when returning (belt & suspenders)

	// force login if no token
	if callCtx.cfg.Token == "" {
		log.Info("No auth token available, trying to log in")
		if err := login(callCtx); err != nil {
			return err
		}
		// note: callCtx.cfg has been updated by login()
	}

	// create http client for the request
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// build HTTP request
	req, err := prepareHTTPRequest(callCtx, client, method, path, body, options.Headers)
	if err != nil {
		return err // assume error messages provide sufficient info
	}

	// execute request, speculatively, assuming the auth token is valid
	callCtx.startSpinner(fmt.Sprintf("Platform API call (%v %v)", req.Method, urlDisplayPath(req.URL)))
	resp, err := client.Do(req)
	if err != nil {
		// nb: spinner will be stopped by defer
		return fmt.Errorf("%v request to %q failed: %w", method, req.URL.String(), err)
	}

	// collect response body (whether success or error)
	var respBytes []byte
	defer resp.Body.Close()
	respBytes, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed reading response to %v to %q (status %v): %w", method, req.URL.String(), resp.StatusCode, err)
	}

	// handle special case when access token needs to be refreshed and request retried
	if resp.StatusCode == http.StatusForbidden {
		callCtx.stopSpinnerHide()
		log.Warn("Current token is no longer valid; trying to refresh")
		err := login(callCtx)
		if err != nil {
			return fmt.Errorf("failed to login: %w", err)
		}

		// note: callCtx.cfg has been updated by login()

		// retry the request
		log.Info("Retrying the request with the refreshed token")
		req, err = prepareHTTPRequest(callCtx, client, method, path, body, options.Headers)
		if err != nil {
			return err // error should have enough context
		}
		callCtx.startSpinner(fmt.Sprintf("Platform API call, retry after login (%v %v)", req.Method, urlDisplayPath(req.URL)))
		resp, err = client.Do(req)
		// leave the spinner until the outcome is finalized, return will stop/fail it
		if err != nil {
			return fmt.Errorf("%v request to %q failed: %w", method, req.URL.String(), err)
		}

		// collect response body (whether success or error)
		defer resp.Body.Close()
		respBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed reading response to %v to %q (status %v): %w", method, req.URL.String(), resp.StatusCode, err)
		}
	}

	// return if API call response indicates error
	// handle 303 in case of updating object that changes the ID
	if resp.StatusCode/100 != 2 && resp.StatusCode != 303 {
		callCtx.stopSpinner(false) // if still running
		if options.ExpectedErrors != nil && slices.Contains(options.ExpectedErrors, resp.StatusCode) {
			log.WithFields(log.Fields{"status": resp.StatusCode}).Info("Platform API call failed with expected error")
		} else {
			log.WithFields(log.Fields{"status": resp.StatusCode}).Error("Platform API call failed")
		}
		return parseIntoError(resp, respBytes)
	}

	// ensure spinner is stopped, API call has succeeded
	callCtx.stopSpinner(true)

	// process body
	contentType := resp.Header.Get("content-type")
	if method != "DELETE" {
		// for downloaded files, save them
		if contentType == "application/octet-stream" || contentType == "application/zip" {
			var solutionFileName = options.Headers["solutionFileName"]
			if solutionFileName == "" {
				return fmt.Errorf("(bug) filename not provided for response type %q", contentType)
			}

			// store the response data into specified file
			err := os.WriteFile(solutionFileName, respBytes, 0777)
			if err != nil {
				return fmt.Errorf("failed to save the solution archive file as %q: %w", solutionFileName, err)
			}
			// if response code is 303 then it won't be valid json
		} else if len(respBytes) > 0 && resp.StatusCode != 303 {
			// unmarshal response from JSON (assuming JSON data, even if the content-type is not set)
			if err := json.Unmarshal(respBytes, out); err != nil {
				return fmt.Errorf("failed to JSON-parse the response: %w (%q)", err, respBytes)
			}
		}
	}

	// return response headers
	if options != nil {
		if resp.Header != nil {
			options.ResponseHeaders = map[string][]string(resp.Header)
		} else {
			options.ResponseHeaders = nil
		}
	}

	return nil
}

// parseError creates an HttpStatusError error from HTTP response data
// This method creates either a simple error with the status code and response body
// or a wrapped Problem struct in case the response is of type "application/problem+json"
func parseIntoError(resp *http.Response, respBytes []byte) error {
	// try various strategies for humanizing the error output, from the most
	// specific to the generic

	// try as "Content-Type: application/problem+json", even if
	// the content type is not set this way (some APIs don't set it)
	var problem Problem
	err := json.Unmarshal(respBytes, &problem)
	if err == nil {
		// set status from http response only if a status is not included in the response body
		if problem.Status == 0 {
			problem.Status = resp.StatusCode
		}
		return &HttpStatusError{Message: http.StatusText(resp.StatusCode), StatusCode: resp.StatusCode, WrappedErr: &problem}
	}

	// attempt to parse response as a generic JSON object
	var errobj any
	err = json.Unmarshal(respBytes, &errobj)
	if err == nil {
		return &HttpStatusError{Message: fmt.Sprintf("status %d, error response: %+v", resp.StatusCode, errobj), StatusCode: resp.StatusCode}
	}

	// fallback to code + response text data; if no text is provided, use the standard status text instead
	text := bytes.NewBuffer(respBytes).String()
	if text == "" {
		text = http.StatusText(resp.StatusCode)
	}
	return &HttpStatusError{Message: fmt.Sprintf("status: %d %v", resp.StatusCode, text), StatusCode: resp.StatusCode}
}

// urlDisplayPath returns the URL path in a display-friendly form (may be abbreviated)
func urlDisplayPath(uri *url.URL) string {
	s := uri.Path
	s = abbreviateString(s, 50)
	if uri.RawQuery == "" {
		return s
	}
	return s + "?" + abbreviateString(uri.RawQuery, 6)
}

// abbreviateString trims the input string to not exceed the specified maximum number of characters.
// If the string is longer than the maximum, it is cut off and an ellipsis rune is added at the end,
// still maintaining the specified maximum length.
func abbreviateString(s string, n uint) string {
	// if the string is short enough, return it as is
	// nb: this handles some edge cases for short s and low n
	if uint(utf8.RuneCountInString(s)) <= n {
		return s
	}

	// handle remaining edge cases
	if n == 0 {
		return ""
	} else if n == 1 {
		return "…"
	}

	// get the first n-1 runes and append ellipsis to make n runes
	runes := []rune(s)
	runes[n-1] = '…'
	return string(runes[:n])
}
