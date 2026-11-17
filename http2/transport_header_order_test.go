package http2

import (
	"fmt"
	"maps"
	mathrand "math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2/hpack"
)

// referenceSortedRequestHeaders is how encodeHeaders put a request's headers
// in order before sortedRequestHeaders: on a clone of the header map, sorted
// by http.Header. It is kept here as what sortedRequestHeaders has to match.
func referenceSortedRequestHeaders(req *http.Request, addGzipHeader bool, contentLength int64) []http.HeaderKeyValues {
	hdrs := req.Header.Clone()
	if hdrs == nil {
		hdrs = make(http.Header)
	}
	for k := range hdrs {
		if strings.EqualFold(k, "content-length") {
			delete(hdrs, k)
		}
	}
	if shouldSendReqContentLength(req.Method, contentLength) {
		hdrs["content-length"] = []string{strconv.FormatInt(contentLength, 10)}
	}
	if _, ok := hdrs["accept-encoding"]; !ok && addGzipHeader {
		hdrs["accept-encoding"] = []string{"gzip, deflate, br, zstd"}
	}

	var kvs []http.HeaderKeyValues
	if headerOrder, ok := hdrs[http.HeaderOrderKey]; ok {
		order := make(map[string]int)
		for i, v := range headerOrder {
			order[v] = i
		}
		kvs, _ = hdrs.SortedKeyValuesBy(order, make(map[string]bool))
	} else {
		kvs, _ = hdrs.SortedKeyValues(make(map[string]bool))
	}

	return append([]http.HeaderKeyValues(nil), kvs...)
}

// TestSortedRequestHeadersMatchesHeaderSort: the order of the headers on the
// wire is part of what a client looks like to a server, so the allocation-free
// sort has to produce exactly what sorting the header map did.
func TestSortedRequestHeadersMatchesHeaderSort(t *testing.T) {
	names := []string{
		"accept", "Accept-Language", "accept-encoding", "Accept-Encoding", "cookie", "Cookie",
		"user-agent", "User-Agent", "sec-ch-ua", "sec-fetch-dest", "X-Requested-With", "x-a", "X-B",
		"content-type", "Content-Length", "content-length", "host", "priority", "referer", "Origin",
	}
	// Names the Header-Order: list is drawn from. An entry that is not lower
	// case never matches a header, and the list may name a header twice.
	orderNames := append([]string{"Accept", "X-B", "not-sent", "content-length", "accept-encoding"}, names...)

	rnd := mathrand.New(mathrand.NewSource(1))
	cc := &ClientConn{}

	for i := 0; i < 5000; i++ {
		h := http.Header{}
		lowered := map[string]bool{}
		for _, n := range rnd.Perm(len(names))[:rnd.Intn(len(names)+1)] {
			name := names[n]
			// Two spellings of one header would tie, and the order of a
			// tie was never defined.
			if lowered[strings.ToLower(name)] {
				continue
			}
			lowered[strings.ToLower(name)] = true
			h[name] = []string{fmt.Sprint("v", n)}
			if rnd.Intn(8) == 0 {
				h[name] = append(h[name], "second")
			}
		}
		switch rnd.Intn(4) {
		case 0: // no Header-Order: key
		case 1:
			h[http.HeaderOrderKey] = []string{}
		default:
			var order []string
			for _, n := range rnd.Perm(len(orderNames))[:rnd.Intn(len(orderNames)+1)] {
				order = append(order, orderNames[n])
			}
			if len(order) > 2 && rnd.Intn(3) == 0 {
				order = append(order, order[0], order[1])
			}
			h[http.HeaderOrderKey] = order
		}
		if rnd.Intn(4) == 0 {
			h[http.PHeaderOrderKey] = []string{":method", ":path"}
		}

		req := &http.Request{Method: []string{"GET", "POST", "PUT", "HEAD"}[rnd.Intn(4)], Header: h}
		if rnd.Intn(20) == 0 {
			req.Header = nil
		}
		addGzip := rnd.Intn(2) == 0
		if _, ok := req.Header["Accept-Encoding"]; ok {
			// The Transport only looks for the lower-case key before it adds
			// its own, so this would send the header under two spellings,
			// which tie.
			addGzip = false
		}
		contentLength := []int64{-1, 0, 7, 123456}[rnd.Intn(4)]

		before := req.Header.Clone()

		want := referenceSortedRequestHeaders(req, addGzip, contentLength)
		fields := cc.sortedRequestHeaders(req, addGzip, contentLength)
		got := make([]http.HeaderKeyValues, 0, len(fields))
		for _, f := range fields {
			got = append(got, http.HeaderKeyValues{Key: f.Key, Values: append([]string(nil), f.Values...)})
		}

		if len(got) != len(want) {
			t.Fatalf("header %v: got %d fields, want %d\n got: %v\nwant: %v", h, len(got), len(want), got, want)
		}
		for j := range want {
			if got[j].Key != want[j].Key || !slices.Equal(got[j].Values, want[j].Values) {
				t.Fatalf("header %v (gzip %v, content length %d): field %d differs\n got: %v\nwant: %v", h, addGzip, contentLength, j, got, want)
			}
		}
		if !maps.EqualFunc(req.Header, before, slices.Equal[[]string]) {
			t.Fatalf("sortedRequestHeaders changed the request's header\n got: %v\nwant: %v", req.Header, before)
		}
	}
}

// TestEncodeHeadersDoesNotKeepTheRequestHeaders: the sorted list is scratch
// space on the connection, and must not keep the last request's header values
// alive after it is encoded.
func TestEncodeHeadersDoesNotKeepTheRequestHeaders(t *testing.T) {
	req, err := http.NewRequest("POST", "https://www.example.org/p", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = http.Header{"a": {"1"}, "b": {"2"}, http.HeaderOrderKey: {"b", "a"}}

	cc := &ClientConn{peerMaxHeaderListSize: 10 << 20, t: &Transport{}}
	cc.henc = hpack.NewEncoder(&cc.hbuf)
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if _, err := cc.encodeHeaders(req, true, "", 5); err != nil {
		t.Fatal(err)
	}

	for i, f := range cc.hdrFields[:cap(cc.hdrFields)] {
		if f.Key != "" || f.Values != nil {
			t.Errorf("scratch field %d still holds %q: %q", i, f.Key, f.Values)
		}
	}
}
