// stall-s3 is a fake S3 endpoint for the stalling-backend lab (Stage 1a
// Prompt 9, Part 0.2): an in-memory, path-style, unauthenticated S3 with
// just the calls the fixed-bucket driver makes (HeadBucket, CreateBucket,
// PutObject, multipart, GetObject with Range, HeadObject, DeleteObject,
// ListObjectsV2), which HOLDS chosen PUTs the way Lyve us-east-1 and
// Wasabi's edge did on 2026-10-17: the whole body is read, then nothing is
// answered until the client gives up. An operator probe: never linked into
// the product.
//
//	stall-s3 -addr 127.0.0.1:9555 -match '_chunks/' -nth 3 -count 1
//	curl '127.0.0.1:9555/_stall/arm?match=partNumber=2&nth=1&count=1'   # re-arm between runs
//	curl '127.0.0.1:9555/_stall/state'
//
// Point a lab server at it with STORAGE_MODE=wasabi,
// WASABI_ENDPOINT=http://127.0.0.1:9555 and any WASABI_ACCESS_KEY /
// WASABI_SECRET_KEY: the fixed-bucket driver is the one whose PUTs take
// deadlinePutClient (the same as lyve's). Every request is one JSON line on
// stdout; a held one says how long it was held and how it ended.
package main

import (
	"bufio"
	"crypto/md5" // #nosec G501 -- S3 ETags are MD5 by definition
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type object struct {
	data        []byte
	etag        string
	contentType string
	meta        http.Header
	modified    time.Time
}

type upload struct {
	key   string
	parts map[int][]byte
}

type stallState struct {
	Match   string `json:"match"`
	Nth     int    `json:"nth"`
	Count   int    `json:"count"`
	Seen    int    `json:"seen"`
	Held    int    `json:"held"`
	Answers int    `json:"answers"` // held requests that got an answer (a -release-after hold)
	// ReleaseAfter answers a held request after this long instead of never
	// (0 = hold until the client gives up).
	ReleaseAfter string `json:"release_after,omitempty"`
}

type fake struct {
	mu      sync.Mutex
	objects map[string]*object // "<bucket>/<key>"
	uploads map[string]*upload
	nextID  int

	re           *regexp.Regexp
	state        stallState
	releaseAfter time.Duration
	out          *json.Encoder
	outMu        sync.Mutex
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9555", "listen address")
	match := flag.String("match", "", "hold PUTs whose path?query matches this regexp (empty = hold none)")
	nth := flag.Int("nth", 1, "hold from the nth matching PUT on")
	count := flag.Int("count", 1, "how many matching PUTs to hold")
	release := flag.Duration("release-after", 0, "answer a held PUT after this long (0 = never: hold until the client gives up)")
	flag.Parse()

	f := &fake{objects: map[string]*object{}, uploads: map[string]*upload{}, out: json.NewEncoder(log.Writer())}
	log.SetFlags(0)
	if err := f.arm(*match, *nth, *count, *release); err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: *addr, Handler: f, ReadHeaderTimeout: 10 * time.Second}
	log.Printf(`{"msg":"stall-s3 listening","addr":%q,"match":%q,"nth":%d,"count":%d}`, *addr, *match, *nth, *count)
	log.Fatal(srv.ListenAndServe())
}

func (f *fake) arm(match string, nth, count int, release time.Duration) error {
	var re *regexp.Regexp
	if match != "" {
		var err error
		if re, err = regexp.Compile(match); err != nil {
			return fmt.Errorf("match: %w", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.re = re
	f.releaseAfter = release
	f.state = stallState{Match: match, Nth: max(nth, 1), Count: count}
	if release > 0 {
		f.state.ReleaseAfter = release.String()
	}
	return nil
}

func (f *fake) event(v map[string]any) {
	v["t"] = time.Now().UTC().Format("15:04:05.000")
	f.outMu.Lock()
	_ = f.out.Encode(v)
	f.outMu.Unlock()
}

// shouldHold decides, under the lock, whether this PUT is one to hold.
func (f *fake) shouldHold(target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.re == nil || !f.re.MatchString(target) {
		return false
	}
	f.state.Seen++
	if f.state.Seen < f.state.Nth || f.state.Held >= f.state.Count {
		return false
	}
	f.state.Held++
	return true
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_stall/") {
		f.control(w, r)
		return
	}
	start := time.Now()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	ev := map[string]any{"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery, "remote": r.RemoteAddr}
	defer func() {
		ev["ms"] = time.Since(start).Milliseconds()
		f.event(ev)
	}()

	switch {
	case key == "" && r.Method == http.MethodHead, key == "" && r.Method == http.MethodPut:
		w.WriteHeader(http.StatusOK) // HeadBucket, CreateBucket
	case key == "" && r.Method == http.MethodGet:
		f.list(w, bucket, q)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("upload-%d", f.nextID)
		f.uploads[id] = &upload{key: bucket + "/" + key, parts: map[int][]byte{}}
		f.mu.Unlock()
		writeXML(w, fmt.Sprintf(`<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, bucket, xmlText(key), id))
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		f.complete(w, r, bucket, key, q.Get("uploadId"), ev)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		f.mu.Lock()
		delete(f.uploads, q.Get("uploadId"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.put(w, r, bucket, key, q, ev)
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		f.get(w, r, bucket+"/"+key)
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, bucket+"/"+key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fake) control(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/_stall/arm":
		q := r.URL.Query()
		nth, _ := strconv.Atoi(q.Get("nth"))
		count, _ := strconv.Atoi(q.Get("count"))
		if q.Get("count") == "" {
			count = 1
		}
		release, _ := time.ParseDuration(q.Get("release-after"))
		if err := f.arm(q.Get("match"), nth, count, release); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "/_stall/state":
	default:
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	st := f.state
	n := len(f.objects)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"state": st, "objects": n})
}

func (f *fake) put(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values, ev map[string]any) {
	data, err := readBody(r)
	ev["bytes"] = len(data)
	if err != nil {
		ev["err"] = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.shouldHold(r.URL.Path + "?" + r.URL.RawQuery) {
		// The 2026-10-17 shape: the whole body taken, then silence.
		ev["held"] = true
		f.event(map[string]any{"msg": "holding", "path": r.URL.Path, "query": r.URL.RawQuery, "bytes": len(data)})
		f.mu.Lock()
		release := f.releaseAfter
		f.mu.Unlock()
		var timer <-chan time.Time
		if release > 0 {
			timer = time.After(release)
		}
		select {
		case <-r.Context().Done():
			ev["held_end"] = "client gave up"
			return
		case <-timer:
			ev["held_end"] = "released"
			f.mu.Lock()
			f.state.Answers++
			f.mu.Unlock()
		}
	}
	sum := md5.Sum(data) // #nosec G401 -- an S3 ETag
	etag := hex.EncodeToString(sum[:])
	f.mu.Lock()
	if id := q.Get("uploadId"); id != "" {
		up, ok := f.uploads[id]
		if !ok {
			f.mu.Unlock()
			s3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		up.parts[n] = data
	} else {
		meta := http.Header{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") || k == "Cache-Control" || k == "Content-Disposition" || k == "Content-Language" {
				meta[k] = v
			}
		}
		f.objects[bucket+"/"+key] = &object{data: data, etag: etag, contentType: r.Header.Get("Content-Type"), meta: meta, modified: time.Now()}
	}
	f.mu.Unlock()
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fake) complete(w http.ResponseWriter, r *http.Request, bucket, key, id string, ev map[string]any) {
	var body struct {
		Parts []struct {
			PartNumber int `xml:"PartNumber"`
		} `xml:"Part"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = xml.Unmarshal(raw, &body)
	f.mu.Lock()
	up, ok := f.uploads[id]
	if !ok {
		f.mu.Unlock()
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	nums := make([]int, 0, len(body.Parts))
	for _, p := range body.Parts {
		nums = append(nums, p.PartNumber)
	}
	sort.Ints(nums)
	var whole []byte
	digests := md5.New() // #nosec G401 -- a multipart ETag
	for _, n := range nums {
		whole = append(whole, up.parts[n]...)
		s := md5.Sum(up.parts[n]) // #nosec G401
		digests.Write(s[:])
	}
	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(digests.Sum(nil)), len(nums))
	f.objects[bucket+"/"+key] = &object{data: whole, etag: etag, modified: time.Now(), meta: http.Header{}}
	delete(f.uploads, id)
	f.mu.Unlock()
	ev["parts"] = len(nums)
	writeXML(w, fmt.Sprintf(`<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"%s"</ETag></CompleteMultipartUploadResult>`, bucket, xmlText(key), etag))
}

func (f *fake) get(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	o, ok := f.objects[id]
	f.mu.Unlock()
	if !ok {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	h := w.Header()
	h.Set("ETag", `"`+o.etag+`"`)
	h.Set("Last-Modified", o.modified.UTC().Format(http.TimeFormat))
	if o.contentType != "" {
		h.Set("Content-Type", o.contentType)
	}
	for k, v := range o.meta {
		h[k] = v
	}
	data, status := o.data, http.StatusOK
	if rg := r.Header.Get("Range"); rg != "" {
		start, end, ok := parseRange(rg, int64(len(o.data)))
		if !ok {
			s3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		data = o.data[start : end+1]
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.data)))
		status = http.StatusPartialContent
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		if e, err := strconv.ParseInt(b, 10, 64); err == nil && e < end {
			end = e
		}
	}
	return start, end, true
}

func (f *fake) list(w http.ResponseWriter, bucket string, q url.Values) {
	prefix := q.Get("prefix")
	f.mu.Lock()
	var b strings.Builder
	keys := make([]string, 0)
	for id := range f.objects {
		if k, ok := strings.CutPrefix(id, bucket+"/"); ok && strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := f.objects[bucket+"/"+k]
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><ETag>"%s"</ETag><Size>%d</Size><LastModified>%s</LastModified></Contents>`,
			xmlText(k), o.etag, len(o.data), o.modified.UTC().Format(time.RFC3339))
	}
	f.mu.Unlock()
	writeXML(w, fmt.Sprintf(`<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><IsTruncated>false</IsTruncated>%s</ListBucketResult>`,
		bucket, xmlText(prefix), len(keys), b.String()))
}

// readBody reads a PUT body, decoding the SDK's aws-chunked framing
// (STREAMING-* payloads: hex size[;ext]\r\n data \r\n … 0\r\n trailers).
func readBody(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("aws-chunked size line: %w", err)
		}
		sizeHex, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		n, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("aws-chunked size %q: %w", sizeHex, err)
		}
		if n == 0 {
			_, _ = io.Copy(io.Discard, br) // trailers
			return out, nil
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, fmt.Errorf("aws-chunked data: %w", err)
		}
		out = append(out, chunk...)
		if _, err := br.Discard(2); err != nil {
			return nil, errors.New("aws-chunked: no CRLF after a chunk")
		}
	}
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
