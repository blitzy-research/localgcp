package gcs

// Protocol-level tests for the Cloud Storage JSON API objects.compose method:
//
//	POST /storage/v1/b/{destinationBucket}/o/{destinationObject}/compose
//
// The endpoint is driven over raw HTTP rather than through the official
// cloud.google.com/go/storage client, for two deliberate reasons:
//
//   - The official client rejects a composer with zero sources before it puts
//     anything on the wire, so the empty-sourceObjects contract is unreachable
//     through the SDK. Posting the body directly is the only way to cover it.
//   - Every other file in this package depends on the Go standard library alone,
//     and these tests keep that property intact.
//
// SDK compatibility for compose is covered separately, out of process, by the
// harness in examples/smoketest, which drives storage.ObjectHandle.ComposerFrom
// against a running binary. That harness also exercises the XML API read path
// (GET /{bucket}/{object}) that the Go client uses for reads; the reads below
// use the JSON media path (?alt=media) instead. Both are served from the same
// store map, so a composed object is readable through either one.
//
// Every helper used here already exists in the package — testServer, postJSON,
// simpleUpload, assertStatus and decodeBody from gcs_test.go, plus the gcpError
// envelope type from errors.go — so this file defines no helper of its own.
// Each test creates its own bucket name so failures stay unambiguous.

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestComposeObject is the acceptance test for the compose contract: two
// uploaded parts are concatenated, in the order requested, into a single
// destination object whose metadata is recomputed over the concatenated bytes.
//
// It also pins destination content-type resolution at both levels reachable
// over HTTP: with no destination.contentType the composite inherits the first
// source's type, and with an explicit destination.contentType that value wins.
func TestComposeObject(t *testing.T) {
	base := testServer(t)

	bucketResp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-bucket"}`)
	assertStatus(t, bucketResp, 200)
	bucketResp.Body.Close()

	// simpleUpload uploads with Content-Type: text/plain, so text/plain is the
	// type the composite must inherit when the request names none.
	first := simpleUpload(t, base, "compose-bucket", "part-1", "Hello, ")
	simpleUpload(t, base, "compose-bucket", "part-2", "world")

	resp := postJSON(t, base+"/storage/v1/b/compose-bucket/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"},{"name":"part-2"}]}`)
	assertStatus(t, resp, 200)

	var obj Object
	decodeBody(t, resp, &obj)

	// The destination name is authoritative from the URL path, not the body.
	if obj.Name != "merged" {
		t.Fatalf("expected name 'merged', got %q", obj.Name)
	}
	if obj.Bucket != "compose-bucket" {
		t.Fatalf("expected bucket 'compose-bucket', got %q", obj.Bucket)
	}
	if obj.Kind != "storage#object" {
		t.Fatalf("expected kind 'storage#object', got %q", obj.Kind)
	}
	// 7 bytes ("Hello, ") + 5 bytes ("world"). Size is a string in the object
	// resource, mirroring the API's uint64-as-string representation.
	if obj.Size != "12" {
		t.Fatalf("expected size 12, got %s", obj.Size)
	}
	if obj.Md5Hash == "" {
		t.Fatal("expected non-empty md5Hash on the composed object")
	}
	if obj.Etag == "" {
		t.Fatal("expected non-empty etag on the composed object")
	}
	if obj.TimeCreated == "" {
		t.Fatal("expected non-empty timeCreated on the composed object")
	}
	if obj.Updated == "" {
		t.Fatal("expected non-empty updated on the composed object")
	}
	if obj.ContentType != first.ContentType {
		t.Fatalf("expected content type %q inherited from the first source, got %q",
			first.ContentType, obj.ContentType)
	}
	if obj.ContentType != "text/plain" {
		t.Fatalf("expected content type 'text/plain', got %q", obj.ContentType)
	}

	// The composite must be the byte-exact, order-preserving concatenation:
	// no separators and no re-encoding.
	mediaResp, err := http.Get(base + "/storage/v1/b/compose-bucket/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	data, err := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if err != nil {
		t.Fatalf("read composed content: %v", err)
	}
	if string(data) != "Hello, world" {
		t.Fatalf("expected 'Hello, world', got %q", string(data))
	}

	// Variant: an explicit destination.contentType outranks the first source's
	// type. The same body carries kind, destination.metadata and
	// deleteSourceObjects, none of which this emulator models — a 200 proves
	// they are accepted and ignored rather than rejected, which is what real
	// clients require. kind in particular must not be mandatory, because the
	// official Go client never transmits it.
	typedResp := postJSON(t, base+"/storage/v1/b/compose-bucket/o/merged-typed/compose",
		`{"kind":"storage#composeRequest","sourceObjects":[{"name":"part-1"},{"name":"part-2"}],`+
			`"destination":{"contentType":"application/json","metadata":{"origin":"compose-test"}},`+
			`"deleteSourceObjects":false}`)
	assertStatus(t, typedResp, 200)

	var typed Object
	decodeBody(t, typedResp, &typed)

	if typed.Name != "merged-typed" {
		t.Fatalf("expected name 'merged-typed', got %q", typed.Name)
	}
	if typed.ContentType != "application/json" {
		t.Fatalf("expected content type 'application/json', got %q", typed.ContentType)
	}
	if typed.Size != "12" {
		t.Fatalf("expected size 12, got %s", typed.Size)
	}

	// deleteSourceObjects is ignored, so the sources must still be readable.
	srcResp, err := http.Get(base + "/storage/v1/b/compose-bucket/o/part-1?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, srcResp, 200)
	srcResp.Body.Close()
}

// TestComposeObjectEmptySources verifies that an empty source list is rejected
// with 400 in the standard GCS JSON error envelope, and that the rejection
// happens before the store is touched so no destination is left behind.
//
// This case cannot be reached through the official Go client, which refuses to
// send a composer with no sources, so raw HTTP is the only way to cover it.
func TestComposeObjectEmptySources(t *testing.T) {
	base := testServer(t)

	bucketResp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-empty"}`)
	assertStatus(t, bucketResp, 200)
	bucketResp.Body.Close()

	resp := postJSON(t, base+"/storage/v1/b/compose-empty/o/merged/compose",
		`{"sourceObjects":[]}`)
	assertStatus(t, resp, 400)

	var errResp gcpError
	decodeBody(t, resp, &errResp)

	if errResp.Error.Code != 400 {
		t.Fatalf("expected code 400, got %d", errResp.Error.Code)
	}
	if errResp.Error.Message == "" {
		t.Fatal("expected non-empty error message")
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

	// A rejected request must not have created the destination.
	check, err := http.Get(base + "/storage/v1/b/compose-empty/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, check, 404)
	check.Body.Close()
}

// TestComposeObjectTooManySources pins the documented source-count boundary as
// inclusive at 32: a 32-entry list succeeds and a 33-entry list is rejected with
// 400. Both lists repeat a single uploaded object, which is legal because Cloud
// Storage permits the same source to appear more than once and the emulator
// performs no de-duplication — every occurrence contributes its bytes.
//
// The final case proves the validation ORDER: a request that is both over-long
// and aimed at a bucket that does not exist must answer 400, not 404, because
// the count check runs before the store is consulted.
func TestComposeObjectTooManySources(t *testing.T) {
	base := testServer(t)

	bucketResp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-limit"}`)
	assertStatus(t, bucketResp, 200)
	bucketResp.Body.Close()

	// One byte per source keeps the expected composite size equal to the number
	// of sources named.
	simpleUpload(t, base, "compose-limit", "part.txt", "x")

	// Guard the contract constant itself, so the boundary assertions below
	// cannot pass vacuously against a wrong limit.
	if maxComposeSourceObjects != 32 {
		t.Fatalf("expected the compose source limit to be 32, got %d", maxComposeSourceObjects)
	}

	entry := `{"name":"part.txt"}`
	atLimit := fmt.Sprintf(`{"sourceObjects":[%s]}`,
		strings.TrimSuffix(strings.Repeat(entry+",", maxComposeSourceObjects), ","))
	overLimit := fmt.Sprintf(`{"sourceObjects":[%s]}`,
		strings.TrimSuffix(strings.Repeat(entry+",", maxComposeSourceObjects+1), ","))

	// 33 sources — one past the limit, so rejected.
	overResp := postJSON(t, base+"/storage/v1/b/compose-limit/o/too-many/compose", overLimit)
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

	// 32 sources — exactly at the limit, so accepted.
	atResp := postJSON(t, base+"/storage/v1/b/compose-limit/o/at-limit/compose", atLimit)
	assertStatus(t, atResp, 200)

	var obj Object
	decodeBody(t, atResp, &obj)
	if obj.Size != "32" {
		t.Fatalf("expected size 32, got %s", obj.Size)
	}

	mediaResp, err := http.Get(base + "/storage/v1/b/compose-limit/o/at-limit?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	data, err := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if err != nil {
		t.Fatalf("read composed content: %v", err)
	}
	if string(data) != strings.Repeat("x", maxComposeSourceObjects) {
		t.Fatalf("expected %d repeated bytes, got %q", maxComposeSourceObjects, string(data))
	}

	// Ordered validation: cardinality precedes store access, so an over-long
	// list aimed at a missing bucket is a 400 and never a 404.
	orderResp := postJSON(t, base+"/storage/v1/b/no-such-bucket/o/too-many/compose", overLimit)
	assertStatus(t, orderResp, 400)
	orderResp.Body.Close()
}

// TestComposeObjectSourceNotFound verifies that a named source which does not
// exist fails the whole request with 404, and — critically — that the operation
// is fail-atomic. Every source is resolved before the single write happens, so
// the destination must not exist afterwards even though the first source
// resolved successfully, and the source that did resolve must be untouched.
func TestComposeObjectSourceNotFound(t *testing.T) {
	base := testServer(t)

	bucketResp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-missing-src"}`)
	assertStatus(t, bucketResp, 200)
	bucketResp.Body.Close()

	// Only the first of the two named sources is uploaded.
	simpleUpload(t, base, "compose-missing-src", "part-1", "present")

	resp := postJSON(t, base+"/storage/v1/b/compose-missing-src/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"},{"name":"absent.txt"}]}`)
	assertStatus(t, resp, 404)

	var errResp gcpError
	decodeBody(t, resp, &errResp)

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

	// Fail-atomicity: no partially composed destination may be observable.
	check, err := http.Get(base + "/storage/v1/b/compose-missing-src/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, check, 404)
	check.Body.Close()

	// The source that resolved must still be intact.
	srcResp, err := http.Get(base + "/storage/v1/b/compose-missing-src/o/part-1?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, srcResp, 200)
	srcData, err := io.ReadAll(srcResp.Body)
	srcResp.Body.Close()
	if err != nil {
		t.Fatalf("read surviving source: %v", err)
	}
	if string(srcData) != "present" {
		t.Fatalf("expected 'present', got %q", string(srcData))
	}
}

// TestComposeObjectBucketNotFound verifies the second 404 condition: the
// destination bucket itself does not exist. The store reports this separately
// from a missing source, which is why compose is a single store-level operation
// rather than handler orchestration over the read-locked getter — that getter
// returns a bare false and cannot tell the two cases apart.
func TestComposeObjectBucketNotFound(t *testing.T) {
	base := testServer(t)

	// No bucket is created. The source list is valid and in range so the request
	// clears cardinality validation and actually reaches the store.
	resp := postJSON(t, base+"/storage/v1/b/no-such-bucket/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"}]}`)
	assertStatus(t, resp, 404)

	var errResp gcpError
	decodeBody(t, resp, &errResp)

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
}

// TestComposeObjectWithSlashesInName verifies that slashes survive the manual
// path routing on both sides of the operation: the sources live under a prefix
// and the destination name contains two slashes. Only the trailing /compose
// token is stripped from the path, so the destination name must round-trip
// exactly and remain readable at its full path.
func TestComposeObjectWithSlashesInName(t *testing.T) {
	base := testServer(t)

	bucketResp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-slash"}`)
	assertStatus(t, bucketResp, 200)
	bucketResp.Body.Close()

	simpleUpload(t, base, "compose-slash", "parts/part-1", "top ")
	simpleUpload(t, base, "compose-slash", "parts/part-2", "level")

	resp := postJSON(t, base+"/storage/v1/b/compose-slash/o/nested/dir/merged/compose",
		`{"sourceObjects":[{"name":"parts/part-1"},{"name":"parts/part-2"}]}`)
	assertStatus(t, resp, 200)

	var obj Object
	decodeBody(t, resp, &obj)

	if obj.Name != "nested/dir/merged" {
		t.Fatalf("expected name 'nested/dir/merged', got %q", obj.Name)
	}
	if obj.Bucket != "compose-slash" {
		t.Fatalf("expected bucket 'compose-slash', got %q", obj.Bucket)
	}
	// 4 bytes ("top ") + 5 bytes ("level").
	if obj.Size != "9" {
		t.Fatalf("expected size 9, got %s", obj.Size)
	}

	mediaResp, err := http.Get(base + "/storage/v1/b/compose-slash/o/nested/dir/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	data, err := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if err != nil {
		t.Fatalf("read composed content: %v", err)
	}
	if string(data) != "top level" {
		t.Fatalf("expected 'top level', got %q", string(data))
	}
}

// TestComposeObjectOverwritesDestination verifies that composing into a name
// that already exists replaces it unconditionally — no existence pre-check and
// no 409, exactly how upload and copy already behave — and that the
// destination's size and hashes are recomputed over the new bytes instead of
// being carried over from the object that was replaced.
func TestComposeObjectOverwritesDestination(t *testing.T) {
	base := testServer(t)

	bucketResp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-overwrite"}`)
	assertStatus(t, bucketResp, 200)
	bucketResp.Body.Close()

	// The destination already exists, with content that is deliberately longer
	// than the composite so a stale size is impossible to miss.
	stale := simpleUpload(t, base, "compose-overwrite", "merged", "stale content that is longer")
	simpleUpload(t, base, "compose-overwrite", "part-1", "fresh ")
	simpleUpload(t, base, "compose-overwrite", "part-2", "bytes")

	resp := postJSON(t, base+"/storage/v1/b/compose-overwrite/o/merged/compose",
		`{"sourceObjects":[{"name":"part-1"},{"name":"part-2"}]}`)
	// 200 and never 409: an existing destination is simply overwritten.
	assertStatus(t, resp, 200)

	var obj Object
	decodeBody(t, resp, &obj)

	// 6 bytes ("fresh ") + 5 bytes ("bytes").
	if obj.Size != "11" {
		t.Fatalf("expected size 11, got %s", obj.Size)
	}
	if obj.Size == stale.Size {
		t.Fatalf("expected the size to be recomputed, still %s", obj.Size)
	}
	if obj.Md5Hash == stale.Md5Hash {
		t.Fatalf("expected md5Hash to be recomputed, still %q", obj.Md5Hash)
	}
	if obj.Etag == stale.Etag {
		t.Fatalf("expected etag to be recomputed, still %q", obj.Etag)
	}

	mediaResp, err := http.Get(base + "/storage/v1/b/compose-overwrite/o/merged?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mediaResp, 200)
	data, err := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if err != nil {
		t.Fatalf("read composed content: %v", err)
	}
	if string(data) != "fresh bytes" {
		t.Fatalf("expected 'fresh bytes', got %q", string(data))
	}
}

// TestComposeObjectMethodNotAllowed is the zero-regression guard for the new
// dispatch branch. Because that branch is gated on POST, every other method on
// the compose path must keep answering exactly as it did before compose existed.
//
// PUT is the method to use here. GET and DELETE are claimed by the
// object-operations switch and answer 404 from the object handlers, whereas PUT
// falls through to that switch's default case and yields the pre-existing 405.
func TestComposeObjectMethodNotAllowed(t *testing.T) {
	base := testServer(t)

	bucketResp := postJSON(t, base+"/storage/v1/b?project=test", `{"name":"compose-method"}`)
	assertStatus(t, bucketResp, 200)
	bucketResp.Body.Close()

	url := base + "/storage/v1/b/compose-method/o/merged/compose"
	req, err := http.NewRequest(http.MethodPut, url, nil)
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	assertStatus(t, resp, 405)

	var errResp gcpError
	decodeBody(t, resp, &errResp)

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
}
