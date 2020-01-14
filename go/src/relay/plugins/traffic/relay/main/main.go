package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"
)

var (
	// This is what the relay will load to handle traffic plugin duties
	Plugin relayPlugin = New()

	hasPort                = regexp.MustCompile(`:\d+$`)
	logger                 = log.New(os.Stdout, "[traffic-relay] ", 0)
	trafficRelayTargetVar  = "TRAFFIC_RELAY_TARGET"
	trafficRelayCookiesVar = "TRAFFIC_RELAY_COOKIES"
)

type relayPlugin struct {
	transport    *http.Transport
	targetScheme string // http|https
	targetHost   string // e.g. 192.168.0.1:1234
}

func New() relayPlugin {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		Proxy:           http.ProxyFromEnvironment,
		IdleConnTimeout: 2 * time.Second, // TODO set from configs
	}
	return relayPlugin{
		transport,
		"",
		"",
	}
}

func (plug relayPlugin) Name() string {
	return "Relay"
}

func (plug relayPlugin) HandleRequest(clientResponse http.ResponseWriter, clientRequest *http.Request, serviced bool) bool {
	if serviced {
		return false
	}
	if plug.targetScheme == "" || plug.targetHost == "" {
		//return false
	}
	if clientRequest.Header.Get("Upgrade") == "websocket" {
		return plug.handleUpgrade(clientResponse, clientRequest)
	} else {
		return plug.handleHttp(clientResponse, clientRequest)
	}
}

func (plug relayPlugin) ConfigVars() map[string]bool {
	return map[string]bool{
		trafficRelayTargetVar:  true,
		trafficRelayCookiesVar: false,
	}
}

func (plug *relayPlugin) Config() bool {
	//cookiesVar := os.Getenv(trafficRelayCookiesVar)
	targetVar := os.Getenv(trafficRelayTargetVar)
	targetURL, err := url.Parse(targetVar)
	if err != nil {
		logger.Printf("Could not parse %v environment variable URL: %v", trafficRelayTargetVar, targetVar)
		return false
	}
	plug.targetScheme = targetURL.Scheme
	plug.targetHost = targetURL.Host
	return true
}

func (plug *relayPlugin) handleHttp(clientResponse http.ResponseWriter, clientRequest *http.Request) bool {
	clientRequest.URL.Scheme = plug.targetScheme
	clientRequest.URL.Host = plug.targetHost
	clientRequest.Host = plug.targetHost
	clientRequest.Header.Set(
		"Origin",
		fmt.Sprintf("%v://%v/", plug.targetScheme, plug.targetHost),
	)
	clientRequest.Header.Del("Cookie") // TODO Handle cookie env var whitelist

	if !clientRequest.URL.IsAbs() {
		logger.Println("Url was not abs", clientRequest.URL.Host)
		http.Error(clientResponse, fmt.Sprintf("This plugin can not respond to non-relay requests: %v", clientRequest.URL), 500)
		return true
	}

	targetResponse, err := plug.transport.RoundTrip(clientRequest)
	if err != nil {
		logger.Printf("Cannot read response from server %v", err)
		return false
	}
	defer targetResponse.Body.Close()

	var bodyReader io.Reader = targetResponse.Body

	// TODO clean up host-specific headers like cookies

	// Set the relayed headers
	for key, values := range targetResponse.Header {
		for _, value := range values {
			clientResponse.Header().Add(key, value)
		}
	}

	if targetResponse.ContentLength > 0 {
		clientResponse.WriteHeader(targetResponse.StatusCode)
		if _, err := io.CopyN(clientResponse, bodyReader, targetResponse.ContentLength); err != nil {
			logger.Printf("Error copying to client: %s", err)
		}
	} else if targetResponse.ContentLength < 0 {
		// The server didn't supply a content length so we calculate one
		body, err := ioutil.ReadAll(bodyReader)
		if err != nil {
			logger.Printf("Cannot read a body: %v", err)
			return true
		}
		clientResponse.Header().Add("Content-Length", strconv.Itoa(int(len(body))))
		clientResponse.WriteHeader(targetResponse.StatusCode)
		if _, err := io.Copy(clientResponse, bytes.NewReader(body)); err != nil {
			logger.Printf("Error copying to client: %s", err)
		}
	} else {
		clientResponse.WriteHeader(targetResponse.StatusCode)
	}
	return true
}

func (plug *relayPlugin) handleUpgrade(clientResponse http.ResponseWriter, clientRequest *http.Request) bool {
	clientRequest.URL.Scheme = plug.targetScheme
	clientRequest.URL.Host = plug.targetHost
	clientRequest.Host = plug.targetHost
	clientRequest.Header.Set(
		"Origin",
		fmt.Sprintf("%v://%v/", plug.targetScheme, plug.targetHost),
	)
	clientRequest.Header.Del("Cookie") // TODO Handle cookie env var whitelist
	// TODO clean up any other host-specific headers

	logger.Println("Upgrading to websocket:", clientRequest.URL)

	// Connect to the target WS service
	var targetConn net.Conn
	var err error
	if clientRequest.URL.Scheme == "https" {
		targetConn, err = tls.Dial("tcp", clientRequest.URL.Host, &tls.Config{
			InsecureSkipVerify: true, // TODO check for cert validity
		})
		if err != nil {
			logger.Println("Error setting up target tls websocket", err)
			http.Error(clientResponse, fmt.Sprintf("Could not dial connect %v", clientRequest.URL.Host, err), 404)
			return true
		}
	} else {
		targetConn, err = net.Dial("tcp", clientRequest.URL.Host)
		if err != nil {
			logger.Println("Error setting up target websocket", err)
			http.Error(clientResponse, fmt.Sprintf("Could not dial connect %v", clientRequest.URL.Host, err), 404)
			return true
		}
	}

	// Write the original client request to the target
	requestLine := fmt.Sprintf("%v %v %v\r\nHost: %v\r\n", clientRequest.Method, clientRequest.URL.String(), clientRequest.Proto, clientRequest.Host)
	if _, err := io.WriteString(targetConn, requestLine); err != nil {
		logger.Printf("Could not write the WS request: %v", err)
		http.Error(clientResponse, fmt.Sprintf("Could not write the WS request: %v %v", clientRequest.URL.Host, err), 500)
		return true
	}
	headerBuffer := new(bytes.Buffer)
	if err := clientRequest.Header.Write(headerBuffer); err != nil {
		logger.Println("Could not write WS header to buffer", err)
		http.Error(clientResponse, fmt.Sprintf("Could not write the WS header: %v %v", clientRequest.URL.Host, err), 500)
		return true
	}
	_, err = headerBuffer.WriteTo(targetConn)
	if err != nil {
		logger.Println("Could not write WS header to target", err)
		http.Error(clientResponse, fmt.Sprintf("Could not write the final header line: %v %v", clientRequest.URL.Host, err), 500)
		return true
	}
	_, err = io.WriteString(targetConn, "\r\n")
	if err != nil {
		logger.Println("Could not complete WS header", err)
		http.Error(clientResponse, fmt.Sprintf("Could not write the final header line: %v %v", clientRequest.URL.Host, err), 500)
		return true
	}

	hij, ok := clientResponse.(http.Hijacker)
	if !ok {
		logger.Println("httpserver does not support hijacking")
		http.Error(clientResponse, "Does not support hijacking", 500)
		return true
	}

	clientConn, _, err := hij.Hijack()
	if err != nil {
		logger.Println("Cannot hijack connection ", err)
		http.Error(clientResponse, "Could not hijack", 500)
		return true
	}

	// And then relay everything between the client and target
	go transfer(targetConn, clientConn)
	transfer(clientConn, targetConn)
	return true
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	io.Copy(destination, source)
}

/*
Copyright 2019 FullStory, Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy of this software
and associated documentation files (the "Software"), to deal in the Software without restriction,
including without limitation the rights to use, copy, modify, merge, publish, distribute,
sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or
substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT
NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY,
WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/
