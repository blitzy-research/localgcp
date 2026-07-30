package gcs

import (
	"bufio"
	"fmt"
	"io"
	"net"
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
// not valid JSON, a source entry with no name, and a body past the byte bound.
// Each returns the shared GCS 400 error envelope, none of them may create or
// modify the destination, and the server keeps serving well-formed requests.
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

	// An oversized sourceObjects array is refused by the same bound, so the cost
	// of decoding it never depends on how many entries it claims to carry.
	entries := strings.Repeat(`{"name":"part-1"},`, 70000)
	oversizedArray := `{"sourceObjects":[` + strings.TrimSuffix(entries, ",") + `]}`
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

	// Neither oversized body may have modified the destination the accepted
	// request composed.
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

	// The server is still responsive after the rejections.
	okResp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"}]}`)
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
// URL and only the trailing /compose token is stripped.
func TestComposeObjectWithSlashesInName(t *testing.T) {
	base := testServer(t)

	resp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-slash"}`)
	assertStatus(t, resp, 200)
	resp.Body.Close()

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

// TestComposeObjectEncodedDestinationName covers destination names in the escaped
// form official clients actually send: url.PathEscape mirrors how a client
// escapes a path parameter, turning every slash inside the name into %2F.
//
// Go's HTTP server percent-decodes the path before the router sees it, so on the
// decoded path a name containing "/copyTo/b/" is indistinguishable from a copy
// sub-operation and a name ending in "/compose" from the compose token itself.
// Dispatching on the escaped path is what keeps those apart: the request below
// used to be served as a cross-bucket copy that answered 200 without composing
// anything, so the assertions cover both the composite and the objects a copy
// would have touched. The copy route itself must keep working, including a copy
// destination that ends in "/compose".
func TestComposeObjectEncodedDestinationName(t *testing.T) {
	base := testServer(t)

	for _, bucket := range []string{"compose-encoded", "compose-decoy"} {
		resp := postJSON(t, base+"/storage/v1/b?project=test", fmt.Sprintf(`{"name":%q}`, bucket))
		assertStatus(t, resp, 200)
		resp.Body.Close()
	}

	simpleUpload(t, base, "compose-encoded", "part-1", "Hello, ")
	simpleUpload(t, base, "compose-encoded", "part-2", "world")
	// "nested" is the source a copy misparsed out of the destination name below,
	// so its bytes surviving unchanged proves no copy was performed.
	simpleUpload(t, base, "compose-encoded", "nested", "decoy source content")

	// The destination name carries the copy marker as data, and names the decoy
	// bucket a misparse would have written into.
	const dstName = "nested/copyTo/b/compose-decoy/o/merged"
	composeResp := postJSON(t,
		base+"/storage/v1/b/compose-encoded/o/"+url.PathEscape(dstName)+"/compose?alt=json&prettyPrint=false",
		`{"sourceObjects":[{"name":"part-1"},{"name":"part-2"}]}`)
	assertStatus(t, composeResp, 200)

	var obj Object
	decodeBody(t, composeResp, &obj)
	if obj.Name != dstName {
		t.Fatalf("expected name %q, got %q", dstName, obj.Name)
	}
	if obj.Bucket != "compose-encoded" {
		t.Fatalf("expected bucket 'compose-encoded', got %q", obj.Bucket)
	}
	if obj.Size != "12" {
		t.Fatalf("expected size 12, got %s", obj.Size)
	}

	// The composite is readable under exactly the name that was requested.
	mediaResp, err := http.Get(base + "/storage/v1/b/compose-encoded/o/" + url.PathEscape(dstName) + "?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	body, _ := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if string(body) != "Hello, world" {
		t.Fatalf("expected 'Hello, world', got %q", string(body))
	}

	// The object a copy would have read is untouched.
	decoyResp, err := http.Get(base + "/storage/v1/b/compose-encoded/o/nested?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, decoyResp, 200)
	decoyBody, _ := io.ReadAll(decoyResp.Body)
	decoyResp.Body.Close()
	if string(decoyBody) != "decoy source content" {
		t.Fatalf("expected the decoy source to keep its bytes, got %q", string(decoyBody))
	}

	// And the bucket named inside the destination was never written to.
	listResp, err := http.Get(base + "/storage/v1/b/compose-decoy/o")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, listResp, 200)

	var list ObjectList
	decodeBody(t, listResp, &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected no cross-bucket write, got %d object(s) in compose-decoy", len(list.Items))
	}

	// A destination whose own last segment is "compose" keeps it: only the
	// structural token is stripped.
	tailResp := postJSON(t,
		base+"/storage/v1/b/compose-encoded/o/"+url.PathEscape("dir/compose")+"/compose",
		`{"sourceObjects":[{"name":"part-1"}]}`)
	assertStatus(t, tailResp, 200)

	var tail Object
	decodeBody(t, tailResp, &tail)
	if tail.Name != "dir/compose" {
		t.Fatalf("expected name 'dir/compose', got %q", tail.Name)
	}

	// The same escaped name without the structural token is not a compose
	// request, so it keeps the pre-existing 405 and composes nothing.
	notComposeResp := postJSON(t, base+"/storage/v1/b/compose-encoded/o/"+url.PathEscape("dir/compose"),
		`{"sourceObjects":[{"name":"part-1"}]}`)
	assertStatus(t, notComposeResp, 405)
	notComposeResp.Body.Close()

	strayResp, err := http.Get(base + "/storage/v1/b/compose-encoded/o/dir")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, strayResp, 404)
	strayResp.Body.Close()

	// Genuine copy requests still reach the copy handler, including one whose
	// destination ends in "/compose".
	copyResp := postJSON(t,
		base+"/storage/v1/b/compose-encoded/o/part-1/copyTo/b/compose-decoy/o/"+url.PathEscape("copied/compose"),
		"{}")
	assertStatus(t, copyResp, 200)

	var copied Object
	decodeBody(t, copyResp, &copied)
	if copied.Bucket != "compose-decoy" || copied.Name != "copied/compose" {
		t.Fatalf("expected copy to compose-decoy/copied/compose, got %s/%s", copied.Bucket, copied.Name)
	}
	if copied.Size != "7" {
		t.Fatalf("expected the copied size 7, got %s", copied.Size)
	}
}

// TestComposeObjectRawEscapedPath pins the escaped path the router dispatches on
// to r.URL.RawPath. When the request target also carries a character net/url
// would have escaped itself — a literal "|" here — RawPath is no longer a
// "valid encoding" in net/url's eyes, so EscapedPath() falls back to re-encoding
// the already-decoded r.URL.Path. That re-encoding turns the destination name's
// %2F back into separators and would hand the request to the copy branch again,
// which is why the verbatim RawPath is preferred whenever it is present.
func TestComposeObjectRawEscapedPath(t *testing.T) {
	base := testServer(t)

	for _, bucket := range []string{"compose-raw", "compose-raw-decoy"} {
		resp := postJSON(t, base+"/storage/v1/b?project=test", fmt.Sprintf(`{"name":%q}`, bucket))
		assertStatus(t, resp, 200)
		resp.Body.Close()
	}

	simpleUpload(t, base, "compose-raw", "part-1", "Hello, ")
	simpleUpload(t, base, "compose-raw", "part-2", "world")
	simpleUpload(t, base, "compose-raw", "nested", "decoy source content")

	// The trailing "|" is appended unescaped, the way a client that escapes only
	// the reserved characters would send it; everything before it is escaped
	// exactly as an official client escapes a path parameter.
	const dstName = "nested/copyTo/b/compose-raw-decoy/o/merged|"
	target := "/storage/v1/b/compose-raw/o/" +
		url.PathEscape("nested/copyTo/b/compose-raw-decoy/o/merged") + "|/compose"

	composeResp := rawPost(t, base, target, `{"sourceObjects":[{"name":"part-1"},{"name":"part-2"}]}`)
	assertStatus(t, composeResp, 200)

	var obj Object
	decodeBody(t, composeResp, &obj)
	if obj.Name != dstName {
		t.Fatalf("expected name %q, got %q", dstName, obj.Name)
	}
	if obj.Bucket != "compose-raw" {
		t.Fatalf("expected bucket 'compose-raw', got %q", obj.Bucket)
	}
	if obj.Size != "12" {
		t.Fatalf("expected size 12, got %s", obj.Size)
	}

	// No copy took place: the object a misparse would have read is unchanged.
	decoyResp, err := http.Get(base + "/storage/v1/b/compose-raw/o/nested?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, decoyResp, 200)
	decoyBody, _ := io.ReadAll(decoyResp.Body)
	decoyResp.Body.Close()
	if string(decoyBody) != "decoy source content" {
		t.Fatalf("expected the decoy source to keep its bytes, got %q", string(decoyBody))
	}

	// And the bucket named inside the destination was never written to.
	listResp, err := http.Get(base + "/storage/v1/b/compose-raw-decoy/o")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, listResp, 200)

	var list ObjectList
	decodeBody(t, listResp, &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected no cross-bucket write, got %d object(s) in compose-raw-decoy", len(list.Items))
	}
}

// rawPost writes a request target verbatim, so characters net/http's client
// would re-escape on the way out survive into the server's request line. The
// parsed response is returned so the shared assertion helpers still apply.
func rawPost(t *testing.T, base, target, body string) *http.Response {
	t.Helper()

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatalf("dial %s: %v", base, err)
	}
	t.Cleanup(func() { conn.Close() })

	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: localgcp\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n\r\n%s", target, len(body), body)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read raw response: %v", err)
	}
	return resp
}
