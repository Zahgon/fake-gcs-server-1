// Copyright 2024 Francisco Souza. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fakestorage

import (
	"context"
	"net/http"
	"regexp"
	"strings"
)

// varsContextKey is the context key under which captured route variables are
// stored on the request.
type varsContextKey struct{}

// Vars returns the route variables captured for the current request, keyed by
// the variable names in the matched route template. The values are the raw,
// still-escaped path segments, matching the behaviour the server relied on when
// it matched paths without cleaning and with the encoded path preserved.
func Vars(r *http.Request) map[string]string {
	if v, ok := r.Context().Value(varsContextKey{}).(map[string]string); ok {
		return v
	}
	return nil
}

// routeMatcher is an extra predicate a request must satisfy for a route to
// match, beyond its method, host and path.
type routeMatcher func(*http.Request) bool

// route is a single registered endpoint. A request matches a route when its
// host and path satisfy the compiled patterns, every extra matcher returns
// true, every required header is present with the given value, and the method
// is one of the accepted methods.
type route struct {
	methods  []string
	headers  [][2]string
	matchers []routeMatcher
	host     *regexp.Regexp
	hostPort bool // host template pins a port; when false the port is ignored
	path     *regexp.Regexp
	handler  http.Handler
}

// router dispatches requests to the first registered route that matches. It
// preserves the encoded request path (no cleaning, no redirect) so that object
// names containing %2F, dot segments, double slashes and unicode reach the
// handlers unchanged.
type router struct {
	routes []*route
}

// handle registers handler for the method set and path template, and returns
// the route so host, header and predicate requirements can be attached.
func (rt *router) handle(path string, handler http.Handler, methods ...string) *route {
	r := &route{
		methods: methods,
		path:    compilePattern(path, "[^/]+"),
		handler: handler,
	}
	rt.routes = append(rt.routes, r)
	return r
}

// handleAny registers handler for the method set with no path constraint; it is
// used by routes that match on host and a predicate alone.
func (rt *router) handleAny(handler http.Handler, methods ...string) *route {
	r := &route{methods: methods, handler: handler}
	rt.routes = append(rt.routes, r)
	return r
}

// withHost restricts the route to requests whose host matches the template.
func (r *route) withHost(host string) *route {
	r.host = compilePattern(host, "[^.]+")
	r.hostPort = strings.Contains(r.host.String(), ":")
	return r
}

// withHeader restricts the route to requests carrying the given header value.
func (r *route) withHeader(key, value string) *route {
	r.headers = append(r.headers, [2]string{key, value})
	return r
}

// withMatcher restricts the route to requests for which m returns true.
func (r *route) withMatcher(m routeMatcher) *route {
	r.matchers = append(r.matchers, m)
	return r
}

// braces captures a single {name} or {name:pattern} template variable.
var braces = regexp.MustCompile(`\{[^{}]+\}`)

// compilePattern turns a route template into an anchored regular expression.
// A bare {name} uses defaultPat; {name:pattern} uses the given pattern. Literal
// text between variables is matched verbatim.
func compilePattern(tmpl, defaultPat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteByte('^')
	last := 0
	for _, loc := range braces.FindAllStringIndex(tmpl, -1) {
		b.WriteString(regexp.QuoteMeta(tmpl[last:loc[0]]))
		name, pat, ok := strings.Cut(tmpl[loc[0]+1:loc[1]-1], ":")
		if !ok {
			pat = defaultPat
		}
		b.WriteString("(?P<")
		b.WriteString(name)
		b.WriteString(">")
		b.WriteString(pat)
		b.WriteByte(')')
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(tmpl[last:]))
	b.WriteByte('$')
	return regexp.MustCompile(b.String())
}

// matchExceptMethod reports whether the request satisfies the route's host,
// predicates, headers and path, returning the captured variables. The method is
// checked separately so a path-but-not-method match can raise 405.
func (r *route) matchExceptMethod(req *http.Request, path string) (map[string]string, bool) {
	vars := map[string]string{}
	if r.host != nil {
		host := requestHost(req)
		if !r.hostPort {
			if i := strings.IndexByte(host, ':'); i != -1 {
				host = host[:i]
			}
		}
		m := r.host.FindStringSubmatch(host)
		if m == nil {
			return nil, false
		}
		collectVars(vars, r.host, m)
	}
	for _, matcher := range r.matchers {
		if !matcher(req) {
			return nil, false
		}
	}
	for _, h := range r.headers {
		if req.Header.Get(h[0]) != h[1] {
			return nil, false
		}
	}
	if r.path != nil {
		m := r.path.FindStringSubmatch(path)
		if m == nil {
			return nil, false
		}
		collectVars(vars, r.path, m)
	}
	return vars, true
}

func (r *route) allowsMethod(method string) bool {
	if len(r.methods) == 0 {
		return true
	}
	for _, m := range r.methods {
		if m == method {
			return true
		}
	}
	return false
}

// requestHost returns the host to match against, preferring the absolute URL's
// host (as set on in-process requests) over the Host header.
func requestHost(req *http.Request) string {
	if req.URL.IsAbs() {
		return req.URL.Host
	}
	return req.Host
}

func collectVars(dst map[string]string, re *regexp.Regexp, match []string) {
	for i, name := range re.SubexpNames() {
		if name != "" {
			dst[name] = match[i]
		}
	}
}

// ServeHTTP dispatches to the first route that matches the request. The encoded
// path is used verbatim. When a route matches the path but not the method, the
// response is 405 with an empty body; when nothing matches it is a plain 404.
func (rt *router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.EscapedPath()
	methodMismatch := false
	for _, r := range rt.routes {
		vars, ok := r.matchExceptMethod(req, path)
		if !ok {
			continue
		}
		if !r.allowsMethod(req.Method) {
			methodMismatch = true
			continue
		}
		req = req.WithContext(context.WithValue(req.Context(), varsContextKey{}, vars))
		r.handler.ServeHTTP(w, req)
		return
	}
	if methodMismatch {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	http.NotFound(w, req)
}
