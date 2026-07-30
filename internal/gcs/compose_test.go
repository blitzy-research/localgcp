package gcs

import (
	"fmt"
	"io"
	"net/http"
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
// not valid JSON, and a source entry with no name. Each returns the shared GCS
// 400 error envelope, and none of them may create the destination.
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
