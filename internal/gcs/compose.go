// Cloud Storage JSON API objects.compose:
//
//	POST /storage/v1/b/{destinationBucket}/o/{destinationObject}/compose
//
// Compose concatenates objects that already live in the destination bucket into
// a single new object, server side, in one call. It backs
// storage.ObjectHandle.ComposerFrom in the Go client, Blob.compose in Python and
// Bucket.combine in Node: all three reduce to this one wire call, so honoring
// the JSON contract once satisfies every SDK.
//
// This file owns the HTTP half of the feature only — path parsing, request
// validation, content-type resolution level 1, delegation, error mapping and
// response serialization. The ordered byte concatenation, the metadata
// recomputation and the atomicity guarantee all live in Store.ComposeObject,
// deliberately: reading the sources here and writing the result back through the
// store would be non-atomic (a concurrent delete landing between the reads and
// the write would yield a partially composed object) and could not tell a
// missing bucket apart from a missing object, because Store.GetObject reports a
// bare false for both.

package gcs

import (
	"encoding/json"
	"net/http"
	"strings"
)

// maxComposeSourceObjects is the greatest number of source objects a single
// compose request may name. Cloud Storage documents a compose request as taking
// "between 1 and 32" objects, and the JSON API reference restates the bound as
// "up to 32 source objects", so 32 is valid and 33 is not.
//
// This constant is the single authority for the value actually enforced; the 400
// messages below restate the same bound in prose for the benefit of humans
// reading the response.
const maxComposeSourceObjects = 32

// composeRequest is the body of an objects.compose request.
//
// Every documented field is optional except sourceObjects. Fields this emulator
// does not model are left undeclared rather than declared-and-unused:
// encoding/json ignores unknown object members (the decoder is used without
// DisallowUnknownFields), so omitting a field is precisely what makes it
// accepted-and-ignored instead of rejected. Each omission below is deliberate.
//
//   - kind ("storage#composeRequest") is neither required nor validated. The
//     official Go client never transmits it and the Discovery Document does not
//     mark it required, so demanding it would break the very clients this
//     emulator exists to serve.
//   - deleteSourceObjects is silently ignored. It is a real field whose
//     documented default is false, so ignoring it is contract-compatible,
//     whereas erroring on it would break conforming clients.
//   - destination.name is ignored because the destination name is authoritative
//     from the URL path. The Go client always sends a non-nil destination but
//     may leave its name empty, since compose requires a destination and the
//     client therefore sets one even for zero-value attributes.
//   - destination.metadata is ignored because Object carries no custom-metadata
//     field and its schema is frozen, so there is nowhere to put it.
//   - The per-source generation and objectPreconditions members are ignored:
//     generation pinning and preconditions are out of scope for this emulator.
type composeRequest struct {
	SourceObjects []composeSourceObject `json:"sourceObjects"`
	Destination   *composeDestination   `json:"destination"`
}

// composeSourceObject names one component of the composite.
//
// Sources are always resolved inside the destination bucket, which is how the
// API rule that every source must reside in the same bucket is enforced
// structurally rather than by validation: a cross-bucket source is
// inexpressible, so no cross-bucket check exists or is needed.
type composeSourceObject struct {
	Name string `json:"name"`
}

// composeDestination carries the destination properties this emulator honors.
//
// composeRequest holds it through a pointer so that an absent "destination"
// member stays distinguishable from one that is present but empty.
type composeDestination struct {
	ContentType string `json:"contentType"`
}

// handleComposeObject serves POST /storage/v1/b/{bucket}/o/{object}/compose.
//
// It receives the prefix-stripped path remainder, matching the shape of
// handleCopyObject: the dispatcher has already trimmed "/storage/v1/b/" from the
// request path, so rest reads as "{bucket}/o/{destinationObject}/compose".
//
// The order of the steps below is part of the contract rather than an
// implementation detail. Every 400 condition is decided before the store is
// consulted, which is what guarantees that a request which is both over-long and
// aimed at a nonexistent bucket answers 400 and not 404.
//
// Query parameters are never consulted. The Go client always appends
// alt=json&prettyPrint=false to a compose request, so interpreting alt as a
// media selector the way handleGetObject does would misread every SDK call;
// destinationPredefinedAcl, dropContextGroups and userProject are likewise
// ignored as ACL, context-group and billing concerns this emulator does not
// model.
//
// On success the destination is returned as a storage#object resource with 200
// implied by Go's first header write. The resource is already contract-correct
// without any schema work, because Object declares no owner or acl fields and
// compose is documented to omit exactly those two.
func (s *Service) handleComposeObject(w http.ResponseWriter, r *http.Request, rest string) {
	// Step 1 — path parsing.
	//
	// The two-way split is what lets a destination name that itself contains
	// "/o/" survive, the same guarantee handleCopyObject relies on. Trimming the
	// suffix removes only the trailing token, so an object legitimately named
	// "x/compose" still resolves: ".../o/x/compose/compose" arrives as
	// "x/compose/compose" and trims back to "x/compose".
	//
	// Nothing is unescaped here. Clients percent-encode slashes inside object
	// names ("a/b" as "a%2Fb"), but net/http decodes the path before the
	// dispatcher sees it, so rest already carries the literal name.
	parts := strings.SplitN(rest, "/o/", 2)
	if len(parts) != 2 {
		writeBadRequest(w, "Invalid compose path")
		return
	}

	bucket := parts[0]
	if bucket == "" {
		writeBadRequest(w, "Destination bucket name is required")
		return
	}

	dstName := strings.TrimSuffix(parts[1], "/compose")
	if dstName == "" {
		writeBadRequest(w, "Destination object name is required")
		return
	}

	// Step 2 — body decoding. Unknown members are read past by design; see the
	// composeRequest documentation for the full list and the reasoning.
	var req composeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid compose request body")
		return
	}

	// Step 3 — cardinality validation, before any store access. Deciding both
	// bounds here is what keeps 400 strictly ahead of 404.
	if len(req.SourceObjects) == 0 {
		writeBadRequest(w, "sourceObjects is required and must name between 1 and 32 source objects")
		return
	}
	if len(req.SourceObjects) > maxComposeSourceObjects {
		writeBadRequest(w, "sourceObjects must name no more than 32 source objects")
		return
	}

	// Step 4 — per-source name validation, folded into the ordered collection of
	// the names passed to the store. An unnamed source cannot be resolved, so it
	// is rejected rather than skipped. Indexed assignment preserves request
	// order exactly: sources are neither sorted nor de-duplicated, so a repeated
	// name contributes its bytes once per appearance, matching Cloud Storage.
	srcNames := make([]string, len(req.SourceObjects))
	for i, src := range req.SourceObjects {
		if src.Name == "" {
			writeBadRequest(w, "Every entry in sourceObjects requires a name")
			return
		}
		srcNames[i] = src.Name
	}

	// Step 5 — content-type resolution, level 1 only.
	//
	// An explicit, non-empty destination.contentType wins here. Anything else
	// passes an empty string down so that Store.ComposeObject can adopt the
	// first source's content type (level 2), falling back in turn to the
	// emulator-wide "application/octet-stream" default applied by newObjectMeta
	// (level 3). Levels 2 and 3 belong to the store and are not duplicated here.
	var contentType string
	if req.Destination != nil {
		contentType = req.Destination.ContentType
	}

	// Step 6 — delegation. One call does the whole read-all-then-write sequence
	// under a single write lock, which is what makes compose atomic and what
	// lets a missing bucket be reported distinctly from a missing source.
	obj, err := s.store.ComposeObject(bucket, dstName, srcNames, contentType)

	// Step 7 — error mapping, the same idiom handleCopyObject and
	// handleDeleteObject use. Store.ComposeObject returns "not found: bucket %q"
	// for an absent destination bucket and "not found: object %q in bucket %q"
	// for an absent source, so both map to 404 through the shared helper; every
	// other failure is genuinely unexpected and surfaces as 500. An existing
	// destination is overwritten unconditionally, so no conflict is ever
	// reported here.
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeNotFound(w, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		}
		return
	}

	// Step 8 — success. 200 is implicit in the first header write.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(obj)
}
