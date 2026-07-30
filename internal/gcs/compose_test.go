package gcs

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// --- objects.compose tests ---
//
// These drive the real Service, mux, handler and store over raw HTTP. Raw HTTP
// is required rather than the official client, because the Go storage client
// rejects a zero-source composer before it sends anything, which would make the
// empty-sourceObjects contract impossible to exercise.

// TestComposeObject is the acceptance test: two uploaded parts concatenate in
// order into one destination with the expected object metadata. It also covers
// the two reachable levels of the content-type fallback and confirms the
// accepted-but-ignored body fields (kind, destination.metadata,
// deleteSourceObjects) do not break the request.
func TestComposeObject(t *testing.T) {
	base := testServer(t)

	resp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-bucket"}`)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	first := simpleUpload(t, base, "compose-bucket", "part-1", "Hello, ")
	simpleUpload(t, base, "compose-bucket", "part-2", "world")

	// The query string carries what real clients always append. It must be
	// ignored rather than interpreted, so alt=json is not mistaken for a media
	// request the way the object GET handler treats alt=media.
	composeResp := postJSON(t, base+"/storage/v1/b/compose-bucket/o/merged/compose?alt=json&prettyPrint=false",
		`{"kind":"storage#composeRequest","sourceObjects":[{"name":"part-1"},{"name":"part-2"}],"destination":{"metadata":{"origin":"compose"}},"deleteSourceObjects":false}`)
	assertStatus(t, composeResp, 200)

	var obj Object
	decodeBody(t, composeResp, &obj)

	if obj.Name != "merged" || obj.Bucket != "compose-bucket" {
		t.Fatalf("unexpected compose result: %+v", obj)
	}
	if obj.Kind != "storage#object" {
		t.Fatalf("expected kind 'storage#object', got %q", obj.Kind)
	}
	if obj.ID != "compose-bucket/merged" {
		t.Fatalf("expected id 'compose-bucket/merged', got %q", obj.ID)
	}
	// 7 bytes of "Hello, " plus 5 bytes of "world"; Size is a string.
	if obj.Size != "12" {
		t.Fatalf("expected size 12, got %s", obj.Size)
	}
	if obj.Md5Hash == "" {
		t.Fatalf("expected recomputed md5Hash, got empty")
	}
	// The hash covers the concatenated bytes rather than being inherited from a
	// source object.
	if obj.Md5Hash == first.Md5Hash {
		t.Fatalf("md5Hash was not recomputed: still %q", obj.Md5Hash)
	}
	if obj.Etag == "" {
		t.Fatalf("expected recomputed etag, got empty")
	}
	if obj.TimeCreated == "" || obj.Updated == "" {
		t.Fatalf("expected timeCreated and updated, got %q and %q", obj.TimeCreated, obj.Updated)
	}
	// No destination content type was supplied, so the first source's wins.
	if obj.ContentType != first.ContentType {
		t.Fatalf("expected content type %q from the first source, got %q", first.ContentType, obj.ContentType)
	}

	mediaResp, err := http.Get(base + "/storage/v1/b/compose-bucket/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	body, _ := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if string(body) != "Hello, world" {
		t.Fatalf("expected 'Hello, world', got %q", string(body))
	}

	typedResp := postJSON(t, base+"/storage/v1/b/compose-bucket/o/merged-typed/compose",
		`{"sourceObjects":[{"name":"part-1"},{"name":"part-2"}],"destination":{"contentType":"application/json"}}`)
	assertStatus(t, typedResp, 200)

	var typed Object
	decodeBody(t, typedResp, &typed)
	if typed.ContentType != "application/json" {
		t.Fatalf("expected content type 'application/json', got %q", typed.ContentType)
	}
	if typed.Size != "12" {
		t.Fatalf("expected size 12, got %s", typed.Size)
	}
}

// TestComposeObjectEmptySources checks the malformed-input paths the handler
// refuses before it ever reaches the store: an empty source list, a body that is
// not valid JSON, a source entry with no name, a body past the byte bound, and a
// body that is not exactly one JSON document. Each returns the shared GCS 400
// error envelope, none of them may create or modify the destination, and the
// server keeps serving well-formed requests afterwards.
func TestComposeObjectEmptySources(t *testing.T) {
	base := testServer(t)

	resp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-empty"}`)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	composeResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose",
		`{"sourceObjects":[]}`)
	assertStatus(t, composeResp, 400)

	var errResp gcpError
	decodeBody(t, composeResp, &errResp)

	if errResp.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", errResp.Error.Code)
	}
	if len(errResp.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(errResp.Error.Errors))
	}
	if errResp.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", errResp.Error.Errors[0].Reason)
	}
	if errResp.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", errResp.Error.Errors[0].Domain)
	}

	// A body that is not valid JSON fails to decode and is refused the same way.
	malformedResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"}`)
	assertStatus(t, malformedResp, 400)

	var malformedErr gcpError
	decodeBody(t, malformedResp, &malformedErr)
	if malformedErr.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", malformedErr.Error.Code)
	}
	if len(malformedErr.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(malformedErr.Error.Errors))
	}
	if malformedErr.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", malformedErr.Error.Errors[0].Reason)
	}
	if malformedErr.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", malformedErr.Error.Errors[0].Domain)
	}

	// An unnamed source cannot be resolved, so it is refused before the store too.
	unnamedResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"},{"name":""}]}`)
	assertStatus(t, unnamedResp, 400)

	var unnamedErr gcpError
	decodeBody(t, unnamedResp, &unnamedErr)
	if unnamedErr.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", unnamedErr.Error.Code)
	}
	if len(unnamedErr.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(unnamedErr.Error.Errors))
	}
	if unnamedErr.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", unnamedErr.Error.Errors[0].Reason)
	}

	// None of the rejected requests may have created the destination.
	missingResp, err := http.Get(base + "/storage/v1/b/compose-empty/o/merged")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, missingResp, 404)
	missingResp.Body.Close()

	// The body is bounded by byte count before it is decoded. The source-count
	// check cannot do that on its own, because it can only run once the whole
	// sourceObjects array has already been materialized. The two bodies below name
	// one legal source and one accepted-and-ignored field, so they are
	// semantically identical and differ only in length: only a byte bound can
	// explain the first composing and the second being refused.
	simpleUpload(t, base, "compose-empty", "part-1", "Hello, ")

	const padTemplate = `{"sourceObjects":[{"name":"part-1"}],"destination":{"metadata":{"pad":%q}}}`
	overhead := len(fmt.Sprintf(padTemplate, ""))

	atLimit := fmt.Sprintf(padTemplate, strings.Repeat("a", maxComposeRequestBytes-overhead))
	if len(atLimit) != maxComposeRequestBytes {
		t.Fatalf("expected a body of exactly %d bytes, got %d", maxComposeRequestBytes, len(atLimit))
	}

	atLimitResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose", atLimit)
	assertStatus(t, atLimitResp, 200)

	var atLimitObj Object
	decodeBody(t, atLimitResp, &atLimitObj)
	if atLimitObj.Size != "7" {
		t.Fatalf("expected size 7, got %s", atLimitObj.Size)
	}

	overLimit := fmt.Sprintf(padTemplate, strings.Repeat("a", maxComposeRequestBytes-overhead+1))
	if len(overLimit) != maxComposeRequestBytes+1 {
		t.Fatalf("expected a body of exactly %d bytes, got %d", maxComposeRequestBytes+1, len(overLimit))
	}

	overLimitResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose", overLimit)
	assertStatus(t, overLimitResp, 400)

	var overLimitErr gcpError
	decodeBody(t, overLimitResp, &overLimitErr)
	if overLimitErr.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", overLimitErr.Error.Code)
	}
	if len(overLimitErr.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(overLimitErr.Error.Errors))
	}
	if overLimitErr.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", overLimitErr.Error.Errors[0].Reason)
	}
	if overLimitErr.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", overLimitErr.Error.Errors[0].Domain)
	}
	if !strings.Contains(overLimitErr.Error.Message, "exceeds the maximum size") {
		t.Fatalf("expected the oversized-body message, got %q", overLimitErr.Error.Message)
	}

	// An oversized sourceObjects array is refused by the same bound, so the cost
	// of decoding it never depends on how many entries it claims to carry.
	oversizedArray := `{"sourceObjects":[` +
		strings.TrimSuffix(strings.Repeat(`{"name":"part-1"},`, 70000), ",") + `]}`
	if len(oversizedArray) <= maxComposeRequestBytes {
		t.Fatalf("expected a body larger than %d bytes, got %d", maxComposeRequestBytes, len(oversizedArray))
	}

	arrayResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose", oversizedArray)
	assertStatus(t, arrayResp, 400)

	var arrayErr gcpError
	decodeBody(t, arrayResp, &arrayErr)
	if arrayErr.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", arrayErr.Error.Code)
	}
	if len(arrayErr.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(arrayErr.Error.Errors))
	}
	if arrayErr.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", arrayErr.Error.Errors[0].Reason)
	}

	// A compose body is exactly one JSON document. A second value must be refused
	// rather than silently ignored, because accepting it would compose from the
	// first value while the request as a whole is malformed — and would leave the
	// rest of the body unvalidated. The first value here is a legal request on its
	// own, so only the end-of-body requirement can reject it.
	twoValueResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/trailing/compose",
		`{"sourceObjects":[{"name":"part-1"}]}{"sourceObjects":[]}`)
	assertStatus(t, twoValueResp, 400)

	var twoValueErr gcpError
	decodeBody(t, twoValueResp, &twoValueErr)
	if twoValueErr.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", twoValueErr.Error.Code)
	}
	if len(twoValueErr.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(twoValueErr.Error.Errors))
	}
	if twoValueErr.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", twoValueErr.Error.Errors[0].Reason)
	}
	if twoValueErr.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", twoValueErr.Error.Errors[0].Domain)
	}

	// Trailing bytes that are not JSON at all are refused the same way.
	garbageResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/trailing/compose",
		`{"sourceObjects":[{"name":"part-1"}]} then some trailing text`)
	assertStatus(t, garbageResp, 400)

	var garbageErr gcpError
	decodeBody(t, garbageResp, &garbageErr)
	if garbageErr.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", garbageErr.Error.Errors[0].Reason)
	}

	// The byte bound covers the WHOLE body, not just the first document: a legal
	// request followed by over-cap padding is refused as oversized even though the
	// document itself is well within the cap.
	const firstDoc = `{"sourceObjects":[{"name":"part-1"}]}`
	overTrailing := firstDoc + strings.Repeat(" ", maxComposeRequestBytes)
	if len(overTrailing) <= maxComposeRequestBytes {
		t.Fatalf("expected a body larger than %d bytes, got %d", maxComposeRequestBytes, len(overTrailing))
	}

	overTrailingResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/trailing/compose", overTrailing)
	assertStatus(t, overTrailingResp, 400)

	var overTrailingErr gcpError
	decodeBody(t, overTrailingResp, &overTrailingErr)
	if len(overTrailingErr.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(overTrailingErr.Error.Errors))
	}
	if !strings.Contains(overTrailingErr.Error.Message, "exceeds the maximum size") {
		t.Fatalf("expected the oversized-body message, got %q", overTrailingErr.Error.Message)
	}

	// None of the malformed-body requests composed anything.
	trailingResp, err := http.Get(base + "/storage/v1/b/compose-empty/o/trailing")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, trailingResp, 404)
	trailingResp.Body.Close()

	// A whitespace-only trailer is NOT a second value and must still be accepted:
	// the official client encodes its body with json.Encoder, which appends a
	// newline, so rejecting trailing whitespace would break every real client.
	whitespaceResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/trailing-ok/compose",
		firstDoc+"\n\n \t\r\n")
	assertStatus(t, whitespaceResp, 200)

	var whitespaceObj Object
	decodeBody(t, whitespaceResp, &whitespaceObj)
	if whitespaceObj.Size != "7" {
		t.Fatalf("expected size 7, got %s", whitespaceObj.Size)
	}

	// Neither oversized body may have modified the destination the accepted
	// request composed, and the server is still responsive after the rejections.
	mediaResp, err := http.Get(base + "/storage/v1/b/compose-empty/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	body, _ := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if string(body) != "Hello, " {
		t.Fatalf("expected 'Hello, ', got %q", string(body))
	}

	okResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose", firstDoc)
	assertStatus(t, okResp, 200)

	var okObj Object
	decodeBody(t, okResp, &okObj)
	if okObj.Size != "7" {
		t.Fatalf("expected size 7, got %s", okObj.Size)
	}
}

// TestComposeObjectTooManySources pins the source limit as inclusive at 32: a
// 33-entry list is rejected with 400 while a 32-entry list succeeds. Duplicate
// source names are permitted, so one upload builds both cases.
func TestComposeObjectTooManySources(t *testing.T) {
	base := testServer(t)

	resp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-limit"}`)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	simpleUpload(t, base, "compose-limit", "part", "x")

	// Guard the contract constant itself, so the boundary assertions below cannot
	// pass vacuously against a wrong limit.
	if maxComposeSourceObjects != 32 {
		t.Fatalf("expected the compose source limit to be 32, got %d", maxComposeSourceObjects)
	}

	entries := make([]string, maxComposeSourceObjects+1)
	for i := range entries {
		entries[i] = `{"name":"part"}`
	}

	overResp := postJSON(t, base+"/storage/v1/b/compose-limit/o/too-many/compose",
		fmt.Sprintf(`{"sourceObjects":[%s]}`, strings.Join(entries, ",")))
	assertStatus(t, overResp, 400)

	var errResp gcpError
	decodeBody(t, overResp, &errResp)

	if errResp.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", errResp.Error.Code)
	}
	if len(errResp.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(errResp.Error.Errors))
	}
	if errResp.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid', got %q", errResp.Error.Errors[0].Reason)
	}
	if errResp.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", errResp.Error.Errors[0].Domain)
	}

	// The rejected request must not have created the destination.
	missingResp, err := http.Get(base + "/storage/v1/b/compose-limit/o/too-many")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, missingResp, 404)
	missingResp.Body.Close()

	atLimitResp := postJSON(t, base+"/storage/v1/b/compose-limit/o/at-limit/compose",
		fmt.Sprintf(`{"sourceObjects":[%s]}`, strings.Join(entries[:maxComposeSourceObjects], ",")))
	assertStatus(t, atLimitResp, 200)

	var obj Object
	decodeBody(t, atLimitResp, &obj)
	if obj.Size != "32" {
		t.Fatalf("expected size 32, got %s", obj.Size)
	}

	// Validation order: the source count is checked before the store is consulted,
	// so a request that is both over-limit and aimed at a nonexistent bucket
	// reports the 400 rather than the 404.
	orderResp := postJSON(t, base+"/storage/v1/b/no-such-bucket/o/too-many/compose",
		fmt.Sprintf(`{"sourceObjects":[%s]}`, strings.Join(entries, ",")))
	assertStatus(t, orderResp, 400)

	var orderErr gcpError
	decodeBody(t, orderResp, &orderErr)
	if len(orderErr.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(orderErr.Error.Errors))
	}
	if orderErr.Error.Errors[0].Reason != "invalid" {
		t.Fatalf("expected reason 'invalid' to win over 'notFound', got %q",
			orderErr.Error.Errors[0].Reason)
	}
}

// TestComposeObjectSourceNotFound checks that a missing source yields 404 and
// that the operation is fail-atomic: the pre-existing destination keeps its
// original bytes, so nothing was partially written.
func TestComposeObjectSourceNotFound(t *testing.T) {
	base := testServer(t)

	resp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-missing-source"}`)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	simpleUpload(t, base, "compose-missing-source", "part-1", "Hello, ")
	// Pre-create the destination so fail-atomicity is observable.
	simpleUpload(t, base, "compose-missing-source", "merged", "original content")

	composeResp := postJSON(t, base+"/storage/v1/b/compose-missing-source/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"},{"name":"absent"}]}`)
	assertStatus(t, composeResp, 404)

	var errResp gcpError
	decodeBody(t, composeResp, &errResp)

	if errResp.Error.Code != 404 {
		t.Fatalf("expected code 404, got %d", errResp.Error.Code)
	}
	if len(errResp.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(errResp.Error.Errors))
	}
	if errResp.Error.Errors[0].Reason != "notFound" {
		t.Fatalf("expected reason 'notFound', got %q", errResp.Error.Errors[0].Reason)
	}
	if errResp.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", errResp.Error.Errors[0].Domain)
	}

	// Fail-atomicity: the destination was neither truncated nor partially written.
	mediaResp, err := http.Get(base + "/storage/v1/b/compose-missing-source/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	body, _ := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if string(body) != "original content" {
		t.Fatalf("expected destination to keep 'original content', got %q", string(body))
	}

	// The source that did resolve must still be intact as well.
	srcResp, err := http.Get(base + "/storage/v1/b/compose-missing-source/o/part-1?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, srcResp, 200)
	srcBody, _ := io.ReadAll(srcResp.Body)
	srcResp.Body.Close()
	if string(srcBody) != "Hello, " {
		t.Fatalf("expected surviving source 'Hello, ', got %q", string(srcBody))
	}
}

// TestComposeObjectBucketNotFound checks that composing into a bucket that does
// not exist yields 404 rather than creating it.
func TestComposeObjectBucketNotFound(t *testing.T) {
	base := testServer(t)

	composeResp := postJSON(t, base+"/storage/v1/b/compose-no-bucket/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"}]}`)
	assertStatus(t, composeResp, 404)

	var errResp gcpError
	decodeBody(t, composeResp, &errResp)

	if errResp.Error.Code != 404 {
		t.Fatalf("expected code 404, got %d", errResp.Error.Code)
	}
	if len(errResp.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(errResp.Error.Errors))
	}
	if errResp.Error.Errors[0].Reason != "notFound" {
		t.Fatalf("expected reason 'notFound', got %q", errResp.Error.Errors[0].Reason)
	}
	if errResp.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", errResp.Error.Errors[0].Domain)
	}

	bucketResp, err := http.Get(base + "/storage/v1/b/compose-no-bucket")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, bucketResp, 404)
	bucketResp.Body.Close()
}

// TestComposeObjectWithSlashesInName checks that a destination name containing
// slashes survives path parsing intact, since the name is authoritative from the
// URL and only the trailing /compose token is stripped. It covers both forms such
// a name arrives in: unescaped slashes, and the percent-escaped form official
// clients actually send — including a name whose own bytes spell a structural
// sub-operation token, which must be treated as data rather than as a copy.
func TestComposeObjectWithSlashesInName(t *testing.T) {
	base := testServer(t)

	for _, bucket := range []string{"compose-slash", "compose-slash-decoy"} {
		resp := postJSON(t, base+"/storage/v1/b?project=test", fmt.Sprintf(`{"name":%q}`, bucket))
		assertStatus(t, resp, 200)
		resp.Body.Close()
	}

	simpleUpload(t, base, "compose-slash", "parts/part-1", "Hello, ")
	simpleUpload(t, base, "compose-slash", "parts/part-2", "world")

	composeResp := postJSON(t, base+"/storage/v1/b/compose-slash/o/nested/dir/merged/compose",
		`{"sourceObjects":[{"name":"parts/part-1"},{"name":"parts/part-2"}]}`)
	assertStatus(t, composeResp, 200)

	var obj Object
	decodeBody(t, composeResp, &obj)
	if obj.Name != "nested/dir/merged" {
		t.Fatalf("expected 'nested/dir/merged', got %q", obj.Name)
	}
	if obj.Bucket != "compose-slash" {
		t.Fatalf("expected bucket 'compose-slash', got %q", obj.Bucket)
	}
	if obj.Size != "12" {
		t.Fatalf("expected size 12, got %s", obj.Size)
	}

	mediaResp, err := http.Get(base + "/storage/v1/b/compose-slash/o/nested/dir/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	body, _ := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if string(body) != "Hello, world" {
		t.Fatalf("expected 'Hello, world', got %q", string(body))
	}

	// url.PathEscape mirrors how an official client escapes a path parameter,
	// turning every slash inside the destination name into %2F. Go's HTTP server
	// decodes the path before the router sees it, so on the decoded path a name
	// containing "/copyTo/b/" is indistinguishable from a copy sub-operation.
	// Deciding structure on the escaped path is what keeps the two apart: this
	// request was previously served as a cross-bucket copy that answered 200
	// without composing anything, so the assertions below cover the composite AND
	// every object a misparsed copy would have read or written.
	//
	// "nested" is the source such a copy misparses out of the destination name,
	// so its bytes surviving unchanged is what proves no copy was performed.
	simpleUpload(t, base, "compose-slash", "nested", "decoy source content")

	const encodedName = "nested/copyTo/b/compose-slash-decoy/o/merged"
	encodedResp := postJSON(t,
		base+"/storage/v1/b/compose-slash/o/"+url.PathEscape(encodedName)+"/compose?alt=json&prettyPrint=false",
		`{"sourceObjects":[{"name":"parts/part-1"},{"name":"parts/part-2"}]}`)
	assertStatus(t, encodedResp, 200)

	var encoded Object
	decodeBody(t, encodedResp, &encoded)
	if encoded.Name != encodedName {
		t.Fatalf("expected name %q, got %q", encodedName, encoded.Name)
	}
	if encoded.Bucket != "compose-slash" {
		t.Fatalf("expected bucket 'compose-slash', got %q", encoded.Bucket)
	}
	if encoded.Size != "12" {
		t.Fatalf("expected size 12, got %s", encoded.Size)
	}

	// The composite is readable under exactly the name that was requested.
	encodedMediaResp, err := http.Get(base + "/storage/v1/b/compose-slash/o/" + url.PathEscape(encodedName) + "?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, encodedMediaResp, 200)
	encodedBody, _ := io.ReadAll(encodedMediaResp.Body)
	encodedMediaResp.Body.Close()
	if string(encodedBody) != "Hello, world" {
		t.Fatalf("expected 'Hello, world', got %q", string(encodedBody))
	}

	// The object a misparsed copy would have read is untouched.
	decoySrcResp, err := http.Get(base + "/storage/v1/b/compose-slash/o/nested?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, decoySrcResp, 200)
	decoySrcBody, _ := io.ReadAll(decoySrcResp.Body)
	decoySrcResp.Body.Close()
	if string(decoySrcBody) != "decoy source content" {
		t.Fatalf("expected the decoy source to keep its bytes, got %q", string(decoySrcBody))
	}

	// And the bucket named inside the destination was never written to.
	decoyListResp, err := http.Get(base + "/storage/v1/b/compose-slash-decoy/o")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, decoyListResp, 200)

	var decoyList ObjectList
	decodeBody(t, decoyListResp, &decoyList)
	if len(decoyList.Items) != 0 {
		t.Fatalf("expected no cross-bucket write, got %d object(s) in compose-slash-decoy", len(decoyList.Items))
	}

	// A destination whose own last segment is "compose" keeps it: only the
	// structural token is stripped.
	tailResp := postJSON(t,
		base+"/storage/v1/b/compose-slash/o/"+url.PathEscape("dir/compose")+"/compose",
		`{"sourceObjects":[{"name":"parts/part-1"}]}`)
	assertStatus(t, tailResp, 200)

	var tail Object
	decodeBody(t, tailResp, &tail)
	if tail.Name != "dir/compose" {
		t.Fatalf("expected name 'dir/compose', got %q", tail.Name)
	}

	// The same escaped name without the structural token is not a compose
	// request, so it keeps its pre-existing 405 and composes nothing.
	notComposeResp := postJSON(t, base+"/storage/v1/b/compose-slash/o/"+url.PathEscape("dir/compose"),
		`{"sourceObjects":[{"name":"parts/part-1"}]}`)
	assertStatus(t, notComposeResp, 405)
	notComposeResp.Body.Close()

	strayResp, err := http.Get(base + "/storage/v1/b/compose-slash/o/dir")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, strayResp, 404)
	strayResp.Body.Close()

	// Genuine copy requests still reach the copy handler unchanged, including one
	// whose escaped source and destination names both contain slashes and whose
	// destination ends in "/compose".
	copyResp := postJSON(t,
		base+"/storage/v1/b/compose-slash/o/"+url.PathEscape("parts/part-1")+
			"/copyTo/b/compose-slash-decoy/o/"+url.PathEscape("copied/compose"),
		"{}")
	assertStatus(t, copyResp, 200)

	var copied Object
	decodeBody(t, copyResp, &copied)
	if copied.Bucket != "compose-slash-decoy" || copied.Name != "copied/compose" {
		t.Fatalf("expected copy to compose-slash-decoy/copied/compose, got %s/%s", copied.Bucket, copied.Name)
	}
	if copied.Size != "7" {
		t.Fatalf("expected the copied size 7, got %s", copied.Size)
	}
}

// TestComposeObjectOverwritesDestination checks that an existing destination is
// replaced unconditionally — no conflict response, and its content and size both
// reflect the composition.
func TestComposeObjectOverwritesDestination(t *testing.T) {
	base := testServer(t)

	resp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-overwrite"}`)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	stale := simpleUpload(t, base, "compose-overwrite", "merged", "stale destination content")
	if stale.Size != "25" {
		t.Fatalf("expected size 25, got %s", stale.Size)
	}
	simpleUpload(t, base, "compose-overwrite", "part-1", "Hello, ")
	simpleUpload(t, base, "compose-overwrite", "part-2", "world")

	composeResp := postJSON(t, base+"/storage/v1/b/compose-overwrite/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"},{"name":"part-2"}]}`)
	assertStatus(t, composeResp, 200)

	var obj Object
	decodeBody(t, composeResp, &obj)
	if obj.Size != "12" {
		t.Fatalf("expected size 12, got %s", obj.Size)
	}
	if obj.Size == stale.Size {
		t.Fatalf("expected a recomputed size, got the stale %s", obj.Size)
	}
	if obj.Md5Hash == stale.Md5Hash {
		t.Fatalf("expected a recomputed md5Hash, got the stale %q", obj.Md5Hash)
	}
	if obj.Etag == stale.Etag {
		t.Fatalf("expected a recomputed etag, got the stale %q", obj.Etag)
	}

	mediaResp, err := http.Get(base + "/storage/v1/b/compose-overwrite/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	body, _ := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if string(body) != "Hello, world" {
		t.Fatalf("expected 'Hello, world', got %q", string(body))
	}
}

// TestComposeObjectMethodNotAllowed verifies that PUT bypasses the POST-only
// compose branch and reaches the generic object-operation 405 path. GET and
// DELETE instead take their object lookup/delete paths and return 404.
func TestComposeObjectMethodNotAllowed(t *testing.T) {
	base := testServer(t)

	resp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-method"}`)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodPut, base+"/storage/v1/b/compose-method/o/merged/compose", nil)
	if err != nil {
		t.Fatal(err)
	}
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, putResp, 405)

	var errResp gcpError
	decodeBody(t, putResp, &errResp)

	if errResp.Error.Code != 405 {
		t.Fatalf("expected code 405, got %d", errResp.Error.Code)
	}
	if len(errResp.Error.Errors) != 1 {
		t.Fatalf("expected 1 error detail, got %d", len(errResp.Error.Errors))
	}
	if errResp.Error.Errors[0].Reason != "methodNotAllowed" {
		t.Fatalf("expected reason 'methodNotAllowed', got %q", errResp.Error.Errors[0].Reason)
	}
	if errResp.Error.Errors[0].Domain != "global" {
		t.Fatalf("expected domain 'global', got %q", errResp.Error.Errors[0].Domain)
	}

	// The /compose suffix is anchored to the object-name segment, so a POST that
	// names no destination object is not a compose request and keeps its
	// pre-existing 405 instead of composing into an object called "compose".
	noObjectResp := postJSON(t, base+"/storage/v1/b/compose-method/o/compose",
		`{"sourceObjects":[{"name":"part-1"}]}`)
	assertStatus(t, noObjectResp, 405)
	noObjectResp.Body.Close()

	noPathResp := postJSON(t, base+"/storage/v1/b/compose-method/compose",
		`{"sourceObjects":[{"name":"part-1"}]}`)
	assertStatus(t, noPathResp, 405)
	noPathResp.Body.Close()

	// Neither shape may have created an object.
	strayResp, err := http.Get(base + "/storage/v1/b/compose-method/o/compose")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, strayResp, 404)
	strayResp.Body.Close()
}
